package push

import (
	"bytes"
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zentra/server/internal/models"
)

const (
	fcmSendEndpoint = "https://fcm.googleapis.com/v1/projects/%s/messages:send"
	fcmTokenURL     = "https://oauth2.googleapis.com/token"
	fcmScope        = "https://www.googleapis.com/auth/firebase.messaging"

	fcmRequestTimeout = 15 * time.Second
	// The whole payload has to fit, the limit is enforced by the OS.
	fcmMaxPayloadBytes = 4096
)

// FCMConfig holds the service account credentials used to reach Firebase.
type FCMConfig struct {
	ProjectID   string
	ClientEmail string
	PrivateKey  string
}

// fcmSender delivers messages through the FCM HTTP v1 API.
//
// Messages are data only. A notification payload would let the SDK display the
// message itself, which bypasses the client's own channel and tap handling, so
// the background isolate stays in control of how a message is presented.
type fcmSender struct {
	config FCMConfig
	key    crypto.PrivateKey
	client *http.Client
	cache  *tokenCache
}

func newFCMSender(config FCMConfig) (*fcmSender, error) {
	key, err := parsePrivateKey(config.PrivateKey)
	if err != nil {
		return nil, err
	}
	return &fcmSender{
		config: config,
		key:    key,
		client: &http.Client{Timeout: fcmRequestTimeout},
		cache:  newTokenCache(),
	}, nil
}

func (s *fcmSender) platform() models.PushPlatform { return models.PushPlatformAndroid }

// accessToken exchanges a self signed assertion for a Google OAuth token.
func (s *fcmSender) accessToken(ctx context.Context) (string, error) {
	return s.cache.get(func() (string, time.Time, error) {
		now := time.Now()
		assertion, err := signJWT(s.key, map[string]string{
			"alg": "RS256",
			"typ": "JWT",
		}, map[string]any{
			"iss":   s.config.ClientEmail,
			"scope": fcmScope,
			"aud":   fcmTokenURL,
			"iat":   now.Unix(),
			"exp":   now.Add(time.Hour).Unix(),
		})
		if err != nil {
			return "", time.Time{}, fmt.Errorf("push: signing fcm assertion: %w", err)
		}

		form := url.Values{}
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
		form.Set("assertion", assertion)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fcmTokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return "", time.Time{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := s.client.Do(req)
		if err != nil {
			return "", time.Time{}, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if err != nil {
			return "", time.Time{}, err
		}
		if resp.StatusCode != http.StatusOK {
			return "", time.Time{}, fmt.Errorf("push: fcm token exchange returned %d: %s", resp.StatusCode, summarize(body))
		}

		var parsed struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return "", time.Time{}, fmt.Errorf("push: decoding fcm token response: %w", err)
		}
		if parsed.AccessToken == "" {
			return "", time.Time{}, errors.New("push: fcm token response contained no access token")
		}

		ttl := time.Duration(parsed.ExpiresIn) * time.Second
		if ttl <= 0 {
			ttl = time.Hour
		}
		return parsed.AccessToken, now.Add(ttl), nil
	})
}

func (s *fcmSender) Send(ctx context.Context, token string, msg Message) error {
	accessToken, err := s.accessToken(ctx)
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf(fcmSendEndpoint, s.config.ProjectID)
	payload, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"token": token,
			"data":  msg.dataPayload(),
			"android": map[string]any{
				// High priority keeps messages arriving immediately during Doze,
				// which is the whole point of using push over polling.
				"priority": "high",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("push: encoding fcm payload: %w", err)
	}
	if len(payload) > fcmMaxPayloadBytes {
		return fmt.Errorf("push: fcm payload is %d bytes, over the %d byte limit", len(payload), fcmMaxPayloadBytes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

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

	if isFCMTokenError(body) {
		return ErrInvalidToken
	}
	return fmt.Errorf("push: fcm send returned %d: %s", resp.StatusCode, summarize(body))
}

// isFCMTokenError reports whether a failure means the token itself is dead and
// the stored device should be dropped.
func isFCMTokenError(body []byte) bool {
	var parsed struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	switch parsed.Error.Status {
	case "UNREGISTERED", "INVALID_ARGUMENT", "SENDER_ID_MISMATCH":
		return true
	default:
		return false
	}
}

func summarize(body []byte) string {
	const max = 256
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > max {
		return trimmed[:max] + "..."
	}
	return trimmed
}
