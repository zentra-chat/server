package messaging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
)

const (
	linkPreviewMaxBytes = 1024 * 1024
	linkPreviewTimeout  = 4 * time.Second
)

var urlRegex = regexp.MustCompile(`https?://[^\s<>()]+|[a-zA-Z0-9-]+(?:\.[a-zA-Z0-9-]+)*\.[a-zA-Z]{2,}(?:/[^\s<>()]*)?`)

func BuildLinkPreviews(ctx context.Context, content string) []models.LinkPreview {
	if strings.TrimSpace(content) == "" {
		return nil
	}

	urlStr := extractFirstURL(content)
	if urlStr == "" {
		return nil
	}

	preview, err := FetchLinkPreview(ctx, urlStr)
	if err != nil {
		log.Debug().Err(err).Str("url", urlStr).Msg("Link preview fetch failed")
		return nil
	}
	if preview == nil {
		return nil
	}

	return []models.LinkPreview{*preview}
}

func extractFirstURL(content string) string {
	match := urlRegex.FindString(content)
	if match == "" {
		return ""
	}

	trimmed := strings.TrimRight(match, ".,;:!?)]\"")

	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") {
		trimmed = "https://" + trimmed
	}

	return trimmed
}

func FetchLinkPreview(ctx context.Context, urlStr string) (*models.LinkPreview, error) {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("unsupported scheme")
	}

	if err := validatePreviewHost(ctx, parsed.Hostname()); err != nil {
		return nil, err
	}

	client := &http.Client{
		Timeout: linkPreviewTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if err := validatePreviewHost(ctx, req.URL.Hostname()); err != nil {
				return err
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ZentraLinkPreview/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("non-200 response")
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(strings.ToLower(contentType), "text/html") {
		return nil, errors.New("unsupported content type")
	}

	limited := io.LimitReader(resp.Body, linkPreviewMaxBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}

	baseURL := resp.Request.URL.String()
	parsedPreview := parseHTMLPreview(baseURL, body)
	if parsedPreview == nil {
		return nil, nil
	}

	parsedPreview.URL = baseURL
	if parsedPreview.SiteName == "" {
		parsedPreview.SiteName = resp.Request.URL.Hostname()
	}

	return parsedPreview, nil
}

func parseHTMLPreview(baseURL string, body []byte) *models.LinkPreview {
	tokenizer := html.NewTokenizer(bytes.NewReader(body))

	var (
		title        string
		description  string
		ogTitle      string
		ogDesc       string
		ogImage      string
		ogSiteName   string
		twitterTitle string
		twitterDesc  string
		twitterImage string
		favicon      string
		baseHref     string
	)

	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				goto done
			}
			return nil
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			switch strings.ToLower(token.Data) {
			case "title":
				if tt == html.StartTagToken {
					if tokenizer.Next() == html.TextToken {
						title = strings.TrimSpace(tokenizer.Token().Data)
					}
				}
			case "base":
				for _, attr := range token.Attr {
					if strings.EqualFold(attr.Key, "href") {
						baseHref = strings.TrimSpace(attr.Val)
					}
				}
			case "meta":
				var name string
				var content string
				for _, attr := range token.Attr {
					switch strings.ToLower(attr.Key) {
					case "name", "property":
						name = strings.ToLower(strings.TrimSpace(attr.Val))
					case "content":
						content = strings.TrimSpace(attr.Val)
					}
				}
				switch name {
				case "description":
					if description == "" {
						description = content
					}
				case "og:title":
					if ogTitle == "" {
						ogTitle = content
					}
				case "og:description":
					if ogDesc == "" {
						ogDesc = content
					}
				case "og:image":
					if ogImage == "" {
						ogImage = content
					}
				case "og:site_name":
					if ogSiteName == "" {
						ogSiteName = content
					}
				case "twitter:title":
					if twitterTitle == "" {
						twitterTitle = content
					}
				case "twitter:description":
					if twitterDesc == "" {
						twitterDesc = content
					}
				case "twitter:image":
					if twitterImage == "" {
						twitterImage = content
					}
				}
			case "link":
				var rel string
				var href string
				for _, attr := range token.Attr {
					switch strings.ToLower(attr.Key) {
					case "rel":
						rel = strings.ToLower(strings.TrimSpace(attr.Val))
					case "href":
						href = strings.TrimSpace(attr.Val)
					}
				}
				if favicon == "" && (rel == "icon" || rel == "shortcut icon" || rel == "apple-touch-icon") {
					favicon = href
				}
			}
		}
	}

done:
	finalTitle := firstNonEmpty(ogTitle, twitterTitle, title)
	finalDesc := firstNonEmpty(ogDesc, twitterDesc, description)
	finalImage := firstNonEmpty(ogImage, twitterImage)
	finalSite := ogSiteName

	preview := &models.LinkPreview{}
	preview.Title = finalTitle
	preview.Description = finalDesc
	preview.SiteName = finalSite

	resolvedBase := baseURL
	if baseHref != "" {
		resolvedBase = resolveURL(baseURL, baseHref)
	}

	preview.ImageURL = resolveURL(resolvedBase, finalImage)
	preview.FaviconURL = resolveURL(resolvedBase, favicon)

	if preview.Title == "" && preview.Description == "" && preview.ImageURL == "" {
		return nil
	}

	return preview
}

func resolveURL(baseURL string, ref string) string {
	if ref == "" {
		return ""
	}
	parsedRef, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if parsedRef.IsAbs() {
		return parsedRef.String()
	}
	parsedBase, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return parsedBase.ResolveReference(parsedRef).String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func validatePreviewHost(ctx context.Context, host string) error {
	if host == "" {
		return errors.New("missing host")
	}

	lowerHost := strings.ToLower(host)
	if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") {
		return errors.New("blocked host")
	}

	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return errors.New("blocked ip")
		}
		return nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	ips, err := net.DefaultResolver.LookupIP(lookupCtx, "ip", host)
	if err != nil {
		return nil
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return errors.New("blocked ip")
		}
	}

	return nil
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() {
		return true
	}

	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1]&0xf0 == 16:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		case ip4[0] == 127:
			return true
		case ip4[0] == 169 && ip4[1] == 254:
			return true
		}
	}

	if ip.IsPrivate() {
		return true
	}

	return false
}
