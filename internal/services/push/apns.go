package push

import (
	"bytes"
	"context"
	"crypto"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/zentra/server/internal/models"
)

const (
	apnsRequestTimeout = 15 * time.Second
	// Apple rejects anything above 4KB for regular notifications.
	apnsMaxPayloadBytes = 4096

	// Provider tokens are valid for an hour but Apple discourages refreshing
	// more than once every 20 minutes, and the cache already refreshes a minute
	// early. 50 minutes sits comfortably inside both limits.
	apnsTokenTTL = 50 * time.Minute
)

// APNSConfig holds the signing key issued by Apple Developer.
type APNSConfig struct {
	// KeyID is the APNs key identifier, the last part of the .p8 filename.
	KeyID string
	// TeamID is the Apple Developer team identifier.
	TeamID string
	// BundleID must match the app's bundle identifier.
	BundleID string
	// PrivateKey is the contents of the AuthKey_*.p8 file.
	PrivateKey string
	// Sandbox selects the development APNs host.
	Sandbox bool
}

// apnsSender delivers messages through the APNs HTTP/2 provider API.
//
// These are alert payloads, so iOS displays them itself. The routing fields ride
// along as custom keys next to aps, which is all the client gets when a tap
// brings the app back.
type apnsSender struct {
	config APNSConfig
	key    crypto.PrivateKey
	client *http.Client
	cache  *tokenCache

	hostOnce sync.Once
	host     string
}

func newAPNSSender(config APNSConfig) (*apnsSender, error) {
	key, err := parsePrivateKey(config.PrivateKey)
	if err != nil {
		return nil, err
	}
	return &apnsSender{
		config: config,
		key:    key,
		client: &http.Client{Timeout: apnsRequestTimeout},
		cache:  newTokenCache(),
	}, nil
}

func (s *apnsSender) platform() models.PushPlatform { return models.PushPlatformIOS }

func (s *apnsSender) baseURL() string {
	s.hostOnce.Do(func() {
		s.host = "https://api.push.apple.com"
		if s.config.Sandbox {
			s.host = "https://api.sandbox.push.apple.com"
		}
	})
	return s.host
}

func (s *apnsSender) providerToken() (string, error) {
	return s.cache.get(func() (string, time.Time, error) {
		now := time.Now()
		token, err := signJWT(s.key, map[string]string{
			"alg": "ES256",
			"kid": s.config.KeyID,
		}, map[string]any{
			"iss": s.config.TeamID,
			"iat": now.Unix(),
		})
		if err != nil {
			return "", time.Time{}, fmt.Errorf("push: signing apns token: %w", err)
		}
		return token, now.Add(apnsTokenTTL), nil
	})
}

func (s *apnsSender) Send(ctx context.Context, token string, msg Message) error {
	providerToken, err := s.providerToken()
	if err != nil {
		return err
	}

	payload := map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{
				"title": msg.Title,
				"body":  msg.Body,
			},
			"sound": "default",
			"badge": 1,
			// No content-available: the alert already wakes the app, and waking
			// it silently as well would make the background handler post a
			// second copy of the same notification.
		},
	}
	for key, value := range msg.dataPayload() {
		payload[key] = value
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("push: encoding apns payload: %w", err)
	}
	if len(encoded) > apnsMaxPayloadBytes {
		return fmt.Errorf("push: apns payload is %d bytes, over the %d byte limit", len(encoded), apnsMaxPayloadBytes)
	}

	endpoint := fmt.Sprintf("%s/3/device/%s", s.baseURL(), token)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "bearer "+providerToken)
	req.Header.Set("Content-Type", "application/json")
	// Without the topic APNs rejects the request outright.
	req.Header.Set("apns-topic", s.config.BundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		return nil
	}

	if isAPNSTokenError(resp.StatusCode, body) {
		return ErrInvalidToken
	}
	return fmt.Errorf("push: apns send returned %d: %s", resp.StatusCode, summarize(body))
}

// isAPNSTokenError reports whether the device token should be pruned. A 410 is
// always terminal. A 400 with one of these reasons means the token was minted
// for a different bundle or environment.
func isAPNSTokenError(status int, body []byte) bool {
	if status == http.StatusGone {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}

	var parsed struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	switch parsed.Reason {
	case "DeviceTokenNotForTopic", "BadDeviceToken", "Unregistered":
		return true
	default:
		return false
	}
}
