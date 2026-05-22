package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
)

const (
	EventTypeNotification     = "NOTIFICATION"
	EventTypeNotificationRead = "NOTIFICATION_READ"
)

var (
	ErrNotFound  = errors.New("notification not found")
	ErrForbidden = errors.New("forbidden")
)

// userMentionRe matches <@UUID> syntax for direct user mentions.
var userMentionRe = regexp.MustCompile(`<@([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})>`)

// roleMentionRe matches <@&UUID> syntax for role mentions.
var roleMentionRe = regexp.MustCompile(`<@&([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})>`)

// everyoneRe matches the literal @everyone token.
var everyoneRe = regexp.MustCompile(`(?:^|\s)@everyone(?:\s|$|[^\w])`)

// hereRe matches the literal @here token.
var hereRe = regexp.MustCompile(`(?:^|\s)@here(?:\s|$|[^\w])`)

// HubInterface is the subset of the WebSocket hub needed by the notification service.
type HubInterface interface {
	SendUserEvent(userID uuid.UUID, eventType string, data any)
	IsUserOnline(userID uuid.UUID) bool
}

// ParsedMention is a single mention extracted from message content.
type ParsedMention struct {
	Type   models.MentionType
	UserID *uuid.UUID // set when Type == MentionTypeUser
	RoleID *uuid.UUID // set when Type == MentionTypeRole
}

// MentionContext carries all context needed to process mentions for a message.
type MentionContext struct {
	ChannelID          uuid.UUID
	MessageID          uuid.UUID
	AuthorID           uuid.UUID
	Content            string
	ReplyToAuthorID    *uuid.UUID // if non-nil, a reply notification is also dispatched
	CanMentionEveryone bool       // true if the author has the MentionEveryone permission
}

// Service handles notification persistence and real-time delivery.
type Service struct {
	db  *pgxpool.Pool
	hub HubInterface
}

func NewService(db *pgxpool.Pool, hub HubInterface) *Service {
	return &Service{db: db, hub: hub}
}

// DMNotificationContext carries context for notifying DM recipients.
type DMNotificationContext struct {
	ConversationID uuid.UUID
	MessageID      uuid.UUID
	SenderID       uuid.UUID
	SenderName     string // display name or username
	Content        string // plaintext for notification body
}

// ProcessDMNotification dispatches a DM_MESSAGE notification to all other
// participants of the conversation. Safe to call in a goroutine.
func (s *Service) ProcessDMNotification(nctx DMNotificationContext) {
	ctx := context.Background()

	participants, err := s.getDMParticipants(ctx, nctx.ConversationID)
	if err != nil {
		log.Error().Err(err).Str("conversationId", nctx.ConversationID.String()).
			Msg("Failed to get DM participants for notification")
		return
	}

	body := truncate(nctx.Content, 200)

	for _, recipientID := range participants {
		if recipientID == nctx.SenderID {
			continue
		}
		s.createAndSend(ctx, models.Notification{
			UserID:    recipientID,
			Type:      models.NotificationTypeDMMessage,
			Title:     nctx.SenderName + " sent you a message",
			Body:      strPtr(body),
			MessageID: uuidPtr(nctx.MessageID),
			ActorID:   uuidPtr(nctx.SenderID),
			Metadata:  map[string]any{"conversationId": nctx.ConversationID.String()},
		})
	}
}

func (s *Service) getDMParticipants(ctx context.Context, conversationID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx,
		`SELECT user_id FROM dm_participants WHERE conversation_id = $1`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate DM participant rows")
	}
	return ids, nil
}

// ---------- Public read/write API ----------

// GetNotifications returns paginated notifications for a user, newest first.
func (s *Service) GetNotifications(ctx context.Context, userID uuid.UUID, limit, offset int) ([]*models.Notification, int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	var total int64
	if err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = $1`, userID,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(ctx, `
		SELECT n.id, n.user_id, n.type, n.title, n.body,
		       n.community_id, n.channel_id, n.message_id, n.actor_id,
		       n.metadata, n.is_read, n.created_at,
		       u.id, u.username, u.display_name, u.avatar_url,
		       u.bio, u.status, u.custom_status, u.created_at
		FROM notifications n
		LEFT JOIN users u ON u.id = n.actor_id
		WHERE n.user_id = $1
		ORDER BY n.created_at DESC
		LIMIT $2 OFFSET $3`,
		userID, limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var notifications []*models.Notification
	for rows.Next() {
		n, err := scanNotificationRow(rows)
		if err != nil {
			log.Error().Err(err).Msg("Failed to scan notification")
			continue
		}
		notifications = append(notifications, n)
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate notification rows")
	}
	if notifications == nil {
		notifications = []*models.Notification{}
	}
	return notifications, total, nil
}

// GetUnreadCount returns the count of unread notifications for a user.
func (s *Service) GetUnreadCount(ctx context.Context, userID uuid.UUID) (int64, error) {
	var count int64
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND is_read = FALSE`, userID,
	).Scan(&count)
	return count, err
}

// MarkRead marks a single notification as read, enforcing ownership.
func (s *Service) MarkRead(ctx context.Context, notifID, userID uuid.UUID) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE notifications SET is_read = TRUE WHERE id = $1 AND user_id = $2`,
		notifID, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	// Tell the client their unread count changed.
	s.sendReadEvent(userID, notifID)
	return nil
}

// MarkAllRead marks every unread notification for a user as read.
func (s *Service) MarkAllRead(ctx context.Context, userID uuid.UUID) error {
	_, err := s.db.Exec(ctx,
		`UPDATE notifications SET is_read = TRUE WHERE user_id = $1 AND is_read = FALSE`,
		userID,
	)
	if err != nil {
		return err
	}
	s.hub.SendUserEvent(userID, EventTypeNotificationRead, map[string]any{"all": true})
	return nil
}

// DeleteNotification removes a notification, enforcing ownership.
func (s *Service) DeleteNotification(ctx context.Context, notifID, userID uuid.UUID) error {
	tag, err := s.db.Exec(ctx,
		`DELETE FROM notifications WHERE id = $1 AND user_id = $2`,
		notifID, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetMessageMentions returns all stored mentions for a given message.
func (s *Service) GetMessageMentions(ctx context.Context, messageID uuid.UUID) ([]*models.MessageMention, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, message_id, channel_id, community_id,
		       author_id, mentioned_user_id, mentioned_role_id, mention_type, created_at
		FROM message_mentions
		WHERE message_id = $1
		ORDER BY created_at ASC`,
		messageID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mentions []*models.MessageMention
	for rows.Next() {
		m := &models.MessageMention{}
		if err := rows.Scan(
			&m.ID, &m.MessageID, &m.ChannelID, &m.CommunityID,
			&m.AuthorID, &m.MentionedUserID, &m.MentionedRoleID, &m.MentionType, &m.CreatedAt,
		); err == nil {
			mentions = append(mentions, m)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate message mention rows")
	}
	if mentions == nil {
		mentions = []*models.MessageMention{}
	}
	return mentions, nil
}

// ---------- Mention processing (designed to run in a goroutine) ----------

// ProcessMessageMentions parses mention tokens from a message and dispatches
// notifications for each recipient. Safe to call in a background goroutine.
func (s *Service) ProcessMessageMentions(mctx MentionContext) {
	ctx := context.Background()

	// Resolve the channel's community so @everyone / @here can target members.
	var communityID *uuid.UUID
	var comID uuid.UUID
	if err := s.db.QueryRow(ctx,
		`SELECT community_id FROM channels WHERE id = $1`, mctx.ChannelID,
	).Scan(&comID); err == nil {
		communityID = &comID
	}

	body := truncate(mctx.Content, 200)

	// notified tracks users already scheduled for a notification on this message.
	notified := map[uuid.UUID]bool{mctx.AuthorID: true}

	for _, mention := range ParseMentions(mctx.Content) {
		switch mention.Type {

		case models.MentionTypeUser:
			if mention.UserID == nil || notified[*mention.UserID] {
				continue
			}
			notified[*mention.UserID] = true

			s.storeMention(ctx, models.MessageMention{
				MessageID:       mctx.MessageID,
				ChannelID:       mctx.ChannelID,
				CommunityID:     communityID,
				AuthorID:        mctx.AuthorID,
				MentionedUserID: mention.UserID,
				MentionType:     models.MentionTypeUser,
			})
			s.createAndSend(ctx, models.Notification{
				UserID:      *mention.UserID,
				Type:        models.NotificationTypeMentionUser,
				Title:       "You were mentioned",
				Body:        strPtr(body),
				CommunityID: communityID,
				ChannelID:   uuidPtr(mctx.ChannelID),
				MessageID:   uuidPtr(mctx.MessageID),
				ActorID:     uuidPtr(mctx.AuthorID),
			})

		case models.MentionTypeRole:
			if mention.RoleID == nil || communityID == nil {
				continue
			}
			roleName, _ := s.getRoleName(ctx, *mention.RoleID)
			members, err := s.getRoleMembers(ctx, *mention.RoleID)
			if err != nil {
				log.Error().Err(err).Str("roleId", mention.RoleID.String()).Msg("Failed to get role members for mention")
				continue
			}

			s.storeMention(ctx, models.MessageMention{
				MessageID:       mctx.MessageID,
				ChannelID:       mctx.ChannelID,
				CommunityID:     communityID,
				AuthorID:        mctx.AuthorID,
				MentionedRoleID: mention.RoleID,
				MentionType:     models.MentionTypeRole,
			})
			for _, uid := range members {
				if notified[uid] {
					continue
				}
				notified[uid] = true
				s.createAndSend(ctx, models.Notification{
					UserID:      uid,
					Type:        models.NotificationTypeMentionRole,
					Title:       fmt.Sprintf("Your role @%s was mentioned", roleName),
					Body:        strPtr(body),
					CommunityID: communityID,
					ChannelID:   uuidPtr(mctx.ChannelID),
					MessageID:   uuidPtr(mctx.MessageID),
					ActorID:     uuidPtr(mctx.AuthorID),
					Metadata:    map[string]any{"roleId": mention.RoleID.String(), "roleName": roleName},
				})
			}

		case models.MentionTypeEveryone:
			if !mctx.CanMentionEveryone || communityID == nil {
				continue
			}
			members, err := s.getCommunityMembers(ctx, *communityID)
			if err != nil {
				log.Error().Err(err).Msg("Failed to get community members for @everyone")
				continue
			}
			s.storeMention(ctx, models.MessageMention{
				MessageID:   mctx.MessageID,
				ChannelID:   mctx.ChannelID,
				CommunityID: communityID,
				AuthorID:    mctx.AuthorID,
				MentionType: models.MentionTypeEveryone,
			})
			for _, uid := range members {
				if notified[uid] {
					continue
				}
				notified[uid] = true
				s.createAndSend(ctx, models.Notification{
					UserID:      uid,
					Type:        models.NotificationTypeMentionEveryone,
					Title:       "@everyone was mentioned",
					Body:        strPtr(body),
					CommunityID: communityID,
					ChannelID:   uuidPtr(mctx.ChannelID),
					MessageID:   uuidPtr(mctx.MessageID),
					ActorID:     uuidPtr(mctx.AuthorID),
				})
			}

		case models.MentionTypeHere:
			if !mctx.CanMentionEveryone || communityID == nil {
				continue
			}
			members, err := s.getCommunityMembers(ctx, *communityID)
			if err != nil {
				log.Error().Err(err).Msg("Failed to get community members for @here")
				continue
			}
			s.storeMention(ctx, models.MessageMention{
				MessageID:   mctx.MessageID,
				ChannelID:   mctx.ChannelID,
				CommunityID: communityID,
				AuthorID:    mctx.AuthorID,
				MentionType: models.MentionTypeHere,
			})
			for _, uid := range members {
				if notified[uid] || !s.hub.IsUserOnline(uid) {
					continue
				}
				notified[uid] = true
				s.createAndSend(ctx, models.Notification{
					UserID:      uid,
					Type:        models.NotificationTypeMentionHere,
					Title:       "@here was mentioned",
					Body:        strPtr(body),
					CommunityID: communityID,
					ChannelID:   uuidPtr(mctx.ChannelID),
					MessageID:   uuidPtr(mctx.MessageID),
					ActorID:     uuidPtr(mctx.AuthorID),
				})
			}
		}
	}

	// Reply notification (send after mention processing so both can't notify same user twice).
	if mctx.ReplyToAuthorID != nil && !notified[*mctx.ReplyToAuthorID] {
		body := truncate(mctx.Content, 200)
		s.createAndSend(ctx, models.Notification{
			UserID:      *mctx.ReplyToAuthorID,
			Type:        models.NotificationTypeReply,
			Title:       "Someone replied to your message",
			Body:        strPtr(body),
			CommunityID: communityID,
			ChannelID:   uuidPtr(mctx.ChannelID),
			MessageID:   uuidPtr(mctx.MessageID),
			ActorID:     uuidPtr(mctx.AuthorID),
		})
	}
}

// ---------- Internal helpers ----------

func (s *Service) createAndSend(ctx context.Context, n models.Notification) {
	n.ID = uuid.New()
	n.IsRead = false
	n.CreatedAt = time.Now()

	metaJSON, _ := json.Marshal(n.Metadata)

	if err := s.db.QueryRow(ctx, `
		INSERT INTO notifications
			(id, user_id, type, title, body,
			 community_id, channel_id, message_id, actor_id,
			 metadata, is_read, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12)
		RETURNING id, created_at`,
		n.ID, n.UserID, n.Type, n.Title, n.Body,
		n.CommunityID, n.ChannelID, n.MessageID, n.ActorID,
		string(metaJSON), n.IsRead, n.CreatedAt,
	).Scan(&n.ID, &n.CreatedAt); err != nil {
		log.Error().Err(err).Str("userId", n.UserID.String()).Msg("Failed to insert notification")
		return
	}

	// Fetch actor for the WS payload.
	if n.ActorID != nil {
		var actor models.PublicUser
		if err := s.db.QueryRow(ctx,
			`SELECT id, username, display_name, avatar_url, bio, status, custom_status, created_at
			 FROM users WHERE id = $1`, *n.ActorID,
		).Scan(&actor.ID, &actor.Username, &actor.DisplayName, &actor.AvatarURL,
			&actor.Bio, &actor.Status, &actor.CustomStatus, &actor.CreatedAt,
		); err == nil {
			n.Actor = &actor
		}
	}

	ptr := n
	s.hub.SendUserEvent(n.UserID, EventTypeNotification, &ptr)
}

func (s *Service) storeMention(ctx context.Context, m models.MessageMention) {
	m.ID = uuid.New()
	m.CreatedAt = time.Now()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO message_mentions
			(id, message_id, channel_id, community_id,
			 author_id, mentioned_user_id, mentioned_role_id, mention_type, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT DO NOTHING`,
		m.ID, m.MessageID, m.ChannelID, m.CommunityID,
		m.AuthorID, m.MentionedUserID, m.MentionedRoleID, m.MentionType, m.CreatedAt,
	); err != nil {
		log.Error().Err(err).Msg("Failed to store message mention")
	}
}

func (s *Service) getRoleMembers(ctx context.Context, roleID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx, `
		SELECT cm.user_id
		FROM member_roles mr
		JOIN community_members cm ON cm.id = mr.member_id
		WHERE mr.role_id = $1`, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate role member rows")
	}
	return ids, nil
}

func (s *Service) getRoleName(ctx context.Context, roleID uuid.UUID) (string, error) {
	var name string
	err := s.db.QueryRow(ctx, `SELECT name FROM roles WHERE id = $1`, roleID).Scan(&name)
	return name, err
}

func (s *Service) getCommunityMembers(ctx context.Context, communityID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx,
		`SELECT user_id FROM community_members WHERE community_id = $1`, communityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate community member rows")
	}
	return ids, nil
}

func (s *Service) sendReadEvent(userID, notifID uuid.UUID) {
	s.hub.SendUserEvent(userID, EventTypeNotificationRead, map[string]any{"id": notifID})
}

// scanNotificationRow scans a row from the notifications LEFT JOIN users query.
func scanNotificationRow(row interface{ Scan(dest ...any) error }) (*models.Notification, error) {
	n := &models.Notification{}
	var metaJSON []byte
	var actorID *uuid.UUID
	var actorUsername *string
	var actorDisplayName, actorAvatarURL, actorBio, actorCustomStatus *string
	var actorStatus *models.UserStatus
	var actorCreatedAt *time.Time

	err := row.Scan(
		&n.ID, &n.UserID, &n.Type, &n.Title, &n.Body,
		&n.CommunityID, &n.ChannelID, &n.MessageID, &n.ActorID,
		&metaJSON, &n.IsRead, &n.CreatedAt,
		&actorID, &actorUsername, &actorDisplayName, &actorAvatarURL,
		&actorBio, &actorStatus, &actorCustomStatus, &actorCreatedAt,
	)
	if err != nil {
		return nil, err
	}

	if len(metaJSON) > 0 && string(metaJSON) != "null" {
		_ = json.Unmarshal(metaJSON, &n.Metadata)
	}

	if actorID != nil && actorUsername != nil {
		n.Actor = &models.PublicUser{
			ID:           *actorID,
			Username:     *actorUsername,
			DisplayName:  actorDisplayName,
			AvatarURL:    actorAvatarURL,
			Bio:          actorBio,
			CustomStatus: actorCustomStatus,
		}
		if actorStatus != nil {
			n.Actor.Status = *actorStatus
		}
		if actorCreatedAt != nil {
			n.Actor.CreatedAt = *actorCreatedAt
		}
	}

	return n, nil
}

// ParseMentions extracts all mention tokens from message content.
// This is exported so the frontend helper or tests can use it directly.
func ParseMentions(content string) []ParsedMention {
	var mentions []ParsedMention
	seen := map[string]bool{}

	// User mentions <@UUID>
	for _, match := range userMentionRe.FindAllStringSubmatch(content, -1) {
		raw := match[1]
		if seen[raw] {
			continue
		}
		seen[raw] = true
		id, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		mentions = append(mentions, ParsedMention{Type: models.MentionTypeUser, UserID: &id})
	}

	// Role mentions <@&UUID>
	for _, match := range roleMentionRe.FindAllStringSubmatch(content, -1) {
		raw := match[1]
		key := "role:" + raw
		if seen[key] {
			continue
		}
		seen[key] = true
		id, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		mentions = append(mentions, ParsedMention{Type: models.MentionTypeRole, RoleID: &id})
	}

	// @everyone
	if !seen["everyone"] && everyoneRe.MatchString(content) {
		seen["everyone"] = true
		mentions = append(mentions, ParsedMention{Type: models.MentionTypeEveryone})
	}

	// @here
	if !seen["here"] && hereRe.MatchString(content) {
		seen["here"] = true
		mentions = append(mentions, ParsedMention{Type: models.MentionTypeHere})
	}

	return mentions
}

// ---------- Micro-helpers ----------

func strPtr(s string) *string         { return &s }
func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
