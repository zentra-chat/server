package push

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
)

var (
	// ErrInvalidToken marks a send that failed because the device token is no
	// longer valid, so the stored device should be removed.
	ErrInvalidToken = errors.New("push: device token is no longer valid")

	// ErrNotFound is returned when a token is not registered for the user.
	ErrNotFound = errors.New("push: device not found")
)

// Message is the notification content handed to a provider.
//
// Body may be empty when the provider should rely on the title alone, which is
// common for short alerts that are clear from the title.
type Message struct {
	Title string
	Body  string
	// NotificationID lets the client deduplicate a push against the one it may
	// have already received over the websocket.
	NotificationID string
	// Type mirrors the stored notification type so the client can route the tap.
	Type string
	// ChannelID or ConversationID is set depending on where the message lives.
	ChannelID      string
	ConversationID string
	CommunityID    string
	MessageID      string
	ActorID        string
}

// Sender is one provider transport, either FCM or APNs.
type Sender interface {
	platform() models.PushPlatform
	Send(ctx context.Context, token string, msg Message) error
}

// PresenceChecker reports whether a user currently holds a live connection.
// Push is skipped for these users because the websocket already delivers the
// notification and the client would otherwise show it twice.
type PresenceChecker interface {
	IsUserOnline(userID uuid.UUID) bool
}

// Config carries credentials for every supported provider. Empty credentials
// leave that provider disabled rather than failing at startup, so a deployment
// without Firebase still runs.
type Config struct {
	FCM  FCMConfig
	APNS APNSConfig
}

// Service owns the device registry and dispatches notifications to platforms.
type Service struct {
	db  *pgxpool.Pool
	hub PresenceChecker
	// Keyed by platform so a user with both an Android phone and an iPhone
	// receives the notification on each.
	senders map[models.PushPlatform]Sender
}

func NewService(db *pgxpool.Pool, hub PresenceChecker, config Config) *Service {
	s := &Service{db: db, hub: hub, senders: map[models.PushPlatform]Sender{}}

	if config.FCM.ProjectID != "" && config.FCM.ClientEmail != "" && config.FCM.PrivateKey != "" {
		sender, err := newFCMSender(config.FCM)
		if err != nil {
			log.Error().Err(err).Msg("Android push is disabled, the FCM service account key could not be parsed")
		} else {
			s.senders[models.PushPlatformAndroid] = sender
			log.Info().Str("project", config.FCM.ProjectID).Msg("Android push enabled via FCM")
		}
	}

	if config.APNS.TeamID != "" && config.APNS.KeyID != "" && config.APNS.PrivateKey != "" {
		sender, err := newAPNSSender(config.APNS)
		if err != nil {
			log.Error().Err(err).Msg("iOS push is disabled, the APNs auth key could not be parsed")
		} else {
			s.senders[models.PushPlatformIOS] = sender
			log.Info().
				Str("bundleId", config.APNS.BundleID).
				Bool("sandbox", config.APNS.Sandbox).
				Msg("iOS push enabled via APNs")
		}
	}

	if len(s.senders) == 0 {
		log.Info().Msg("Push notifications are not configured, background delivery is disabled")
	}
	return s
}

// Enabled reports whether any provider is available to deliver pushes.
func (s *Service) Enabled() bool {
	return len(s.senders) > 0
}

// Register stores a device token for a user, replacing any prior owner.
//
// Tokens can migrate between accounts on a shared device, so an upsert on the
// token column reassigns the device rather than creating a duplicate.
func (s *Service) Register(ctx context.Context, userID uuid.UUID, platform models.PushPlatform, token, deviceName, appVersion string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("push: token is required")
	}
	if platform != models.PushPlatformAndroid && platform != models.PushPlatformIOS {
		return fmt.Errorf("push: unsupported platform %q", platform)
	}

	if _, err := s.db.Exec(ctx, `
		INSERT INTO push_devices (id, user_id, platform, token, device_name, app_version)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (token) DO UPDATE
		SET user_id = EXCLUDED.user_id,
		    platform = EXCLUDED.platform,
		    device_name = EXCLUDED.device_name,
		    app_version = EXCLUDED.app_version,
		    last_seen_at = NOW()`,
		uuid.New(), userID, platform, token, nullIfEmpty(deviceName), nullIfEmpty(appVersion),
	); err != nil {
		return err
	}
	return nil
}

// Unregister removes a device token, enforcing ownership so one user cannot
// discard another user's device.
func (s *Service) Unregister(ctx context.Context, userID uuid.UUID, token string) error {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM push_devices WHERE token = $1 AND user_id = $2`, token, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListDevices returns the devices registered for a user.
func (s *Service) ListDevices(ctx context.Context, userID uuid.UUID) ([]*models.PushDevice, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, user_id, platform, token, device_name, app_version, last_seen_at, created_at
		FROM push_devices
		WHERE user_id = $1
		ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	devices := []*models.PushDevice{}
	for rows.Next() {
		d := &models.PushDevice{}
		if err := rows.Scan(
			&d.ID, &d.UserID, &d.Platform, &d.Token,
			&d.DeviceName, &d.AppVersion, &d.LastSeenAt, &d.CreatedAt,
		); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate push device rows")
	}
	return devices, nil
}

// Notify delivers a notification to every device a user has registered.
//
// It returns without doing anything when the user is connected, since the
// websocket already delivered the same notification to the running app.
func (s *Service) Notify(ctx context.Context, userID uuid.UUID, msg Message) {
	if len(s.senders) == 0 {
		return
	}
	if s.hub != nil && s.hub.IsUserOnline(userID) {
		return
	}

	// Notification processing runs on a background goroutine, so the incoming
	// context may already be cancelled. Give the sends their own deadline.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	devices, err := s.ListDevices(ctx, userID)
	if err != nil {
		log.Warn().Err(err).Str("userId", userID.String()).Msg("Failed to load push devices for notification")
		return
	}

	for _, device := range devices {
		sender, ok := s.senders[device.Platform]
		if !ok {
			continue
		}

		s.send(ctx, userID, device, sender, msg)
	}
}

func (s *Service) send(ctx context.Context, userID uuid.UUID, device *models.PushDevice, sender Sender, msg Message) {
	// Touch the device so last_seen_at reflects real usage, which makes
	// pruning stale registrations possible later.
	if _, err := s.db.Exec(ctx,
		`UPDATE push_devices SET last_seen_at = NOW() WHERE id = $1`, device.ID,
	); err != nil {
		log.Warn().Err(err).Str("deviceId", device.ID.String()).Msg("Failed to update push device last seen")
	}

	err := sender.Send(ctx, device.Token, msg)
	if err == nil {
		return
	}

	if !errors.Is(err, ErrInvalidToken) {
		log.Warn().Err(err).
			Str("userId", userID.String()).
			Str("platform", string(device.Platform)).
			Msg("Failed to send push notification")
		return
	}

	log.Info().
		Str("userId", userID.String()).
		Str("platform", string(device.Platform)).
		Msg("Removing push device with a rejected token")
	if _, delErr := s.db.Exec(ctx,
		`DELETE FROM push_devices WHERE id = $1`, device.ID,
	); delErr != nil {
		log.Warn().Err(delErr).Str("deviceId", device.ID.String()).Msg("Failed to delete invalid push device")
	}
}

// dataPayload flattens a message into the string map both providers accept as
// their custom data. FCM rejects non string values in data payloads, so
// everything is stringified and absent fields are left out entirely.
func (m Message) dataPayload() map[string]string {
	payload := map[string]string{"title": m.Title}
	fields := map[string]string{
		"type":           m.Type,
		"notificationId": m.NotificationID,
		"channelId":      m.ChannelID,
		"conversationId": m.ConversationID,
		"communityId":    m.CommunityID,
		"messageId":      m.MessageID,
		"actorId":        m.ActorID,
	}
	if m.Body != "" {
		fields["body"] = m.Body
	}
	for key, value := range fields {
		if value != "" {
			payload[key] = value
		}
	}
	return payload
}

func nullIfEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
