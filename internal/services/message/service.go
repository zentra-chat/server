package message

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
	"github.com/zentra/server/internal/services/messaging"
	"github.com/zentra/server/internal/services/notification"
	"github.com/zentra/server/internal/utils"
)

var (
	ErrMessageNotFound   = errors.New("message not found")
	ErrInsufficientPerms = errors.New("insufficient permissions")
	ErrNotMessageOwner   = errors.New("not message owner")
	ErrCannotEdit        = errors.New("cannot edit this message")
	ErrInvalidReaction   = errors.New("invalid reaction")
)

type Service struct {
	db                  *pgxpool.Pool
	redis               *redis.Client
	channelService      ChannelServiceInterface
	notificationService *notification.Service
	cipher              messaging.ContentCipher
}

type ChannelServiceInterface interface {
	CanAccessChannel(ctx context.Context, channelID, userID uuid.UUID) bool
	CanSendMessage(ctx context.Context, channelID, userID uuid.UUID) bool
	CanManageMessages(ctx context.Context, channelID, userID uuid.UUID) bool
	CanPinMessages(ctx context.Context, channelID, userID uuid.UUID) bool
	CanMentionEveryone(ctx context.Context, channelID, userID uuid.UUID) bool
}

func NewService(db *pgxpool.Pool, redis *redis.Client, encryptionKey []byte, channelService ChannelServiceInterface) *Service {
	return &Service{
		db:             db,
		redis:          redis,
		channelService: channelService,
		cipher:         messaging.NewChannelCipher(encryptionKey),
	}
}

// SetNotificationService wires the notification service into the message service after
// both have been created (the hub is needed by the notification service, which is
// initialised after the message service in main).
func (s *Service) SetNotificationService(ns *notification.Service) {
	s.notificationService = ns
}

// Request/Response types
type CreateMessageRequest struct {
	Content     string      `json:"content" validate:"required_without=Attachments,max=4000"`
	ReplyToID   *uuid.UUID  `json:"replyToId,omitempty"`
	Attachments []uuid.UUID `json:"attachments,omitempty" validate:"max=10"`
}

type UpdateMessageRequest struct {
	Content string `json:"content" validate:"required,max=4000"`
}

type MessageResponse struct {
	*models.Message
	Author      *models.PublicUser         `json:"author"`
	Attachments []models.MessageAttachment `json:"attachments,omitempty"`
	Reactions   []ReactionSummary          `json:"reactions,omitempty"`
	ReplyTo     *MessageReplyPreview       `json:"replyTo,omitempty"`
}

type MessageReplyPreview struct {
	ID       uuid.UUID          `json:"id"`
	Content  string             `json:"content"`
	AuthorID uuid.UUID          `json:"authorId"`
	Author   *models.PublicUser `json:"author"`
}

type ReactionSummary struct {
	Emoji   string      `json:"emoji"`
	Count   int         `json:"count"`
	Users   []uuid.UUID `json:"users"`
	Reacted bool        `json:"reacted"`
}

type GetMessagesParams struct {
	Before *uuid.UUID
	After  *uuid.UUID
	Limit  int
}

func (s *Service) broadcast(ctx context.Context, channelID string, eventType string, data interface{}) {
	event := struct {
		Type string      `json:"type"`
		Data interface{} `json:"data"`
	}{
		Type: eventType,
		Data: data,
	}

	broadcast := struct {
		ChannelID string      `json:"channelId"`
		Event     interface{} `json:"event"`
	}{
		ChannelID: channelID,
		Event:     event,
	}

	jsonData, err := json.Marshal(broadcast)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal message broadcast")
		return
	}

	err = s.redis.Publish(ctx, "websocket:broadcast", jsonData).Err()
	if err != nil {
		log.Error().Err(err).Msg("Failed to publish message broadcast to Redis")
	}
}

// CreateMessage creates a new message in a channel
func (s *Service) CreateMessage(ctx context.Context, channelID, userID uuid.UUID, req *CreateMessageRequest) (*MessageResponse, error) {
	if !s.channelService.CanSendMessage(ctx, channelID, userID) {
		return nil, ErrInsufficientPerms
	}

	linkPreviews := messaging.BuildLinkPreviews(ctx, req.Content)
	linkPreviewJSON := messaging.EncodeLinkPreviews(linkPreviews)

	// Encrypt message content
	encryptedContent, _, err := s.cipher.Encrypt(req.Content)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt message: %w", err)
	}

	messageID := uuid.New()
	now := time.Now()

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Insert message
	query := `
		INSERT INTO messages (id, channel_id, author_id, encrypted_content, reply_to_id, link_previews, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $7)
		RETURNING id, channel_id, author_id, encrypted_content, reply_to_id, link_previews, is_pinned, is_edited, created_at, updated_at`

	var msg models.Message
	var encContent []byte
	var linkPreviewRaw []byte
	err = tx.QueryRow(ctx, query,
		messageID, channelID, userID, encryptedContent, req.ReplyToID, string(linkPreviewJSON), now,
	).Scan(
		&msg.ID, &msg.ChannelID, &msg.AuthorID, &encContent,
		&msg.ReplyToID, &linkPreviewRaw, &msg.IsPinned, &msg.IsEdited, &msg.CreatedAt, &msg.UpdatedAt,
	)
	if err != nil {
		log.Error().Err(err).Msg("Failed to insert message")
		return nil, err
	}
	msg.LinkPreviews = messaging.DecodeLinkPreviews(linkPreviewRaw)

	// Decrypt for response
	contentStr, err := s.cipher.Decrypt(encContent, nil)
	if err != nil {
		log.Error().Err(err).Msg("Failed to decrypt message after insert")
		return nil, err
	}
	msg.Content = &contentStr

	// Link attachments to message
	if len(req.Attachments) > 0 {
		for _, attachmentID := range req.Attachments {
			_, err = tx.Exec(ctx,
				`UPDATE message_attachments SET message_id = $1 WHERE id = $2`,
				messageID, attachmentID,
			)
			if err != nil {
				log.Error().Err(err).Msg("Failed to link attachment")
				return nil, err
			}
		}
	}

	// Update channel's last message
	_, err = tx.Exec(ctx,
		`UPDATE channels SET last_message_at = $1 WHERE id = $2`,
		now, channelID,
	)
	if err != nil {
		log.Error().Err(err).Msg("Failed to update channel last_message_at")
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		log.Error().Err(err).Msg("Failed to commit message transaction")
		return nil, err
	}

	// Fetch complete response
	resp, err := s.GetMessage(ctx, messageID, userID)
	if err != nil {
		log.Error().Err(err).Msg("Failed to fetch message after creation")
		return nil, err
	}

	// Broadcast to WebSocket clients
	s.broadcast(ctx, channelID.String(), "MESSAGE_CREATE", resp)

	// Dispatch mention and reply notifications asynchronously.
	if s.notificationService != nil && req.Content != "" {
		var replyToAuthorID *uuid.UUID
		if resp.ReplyTo != nil {
			replyToAuthorID = &resp.ReplyTo.AuthorID
		}
		canMention := s.channelService.CanMentionEveryone(ctx, channelID, userID)
		mctx := notification.MentionContext{
			ChannelID:          channelID,
			MessageID:          messageID,
			AuthorID:           userID,
			Content:            req.Content,
			ReplyToAuthorID:    replyToAuthorID,
			CanMentionEveryone: canMention,
		}
		go s.notificationService.ProcessMessageMentions(mctx)
	}

	return resp, nil
}

// GetMessage retrieves a single message
func (s *Service) GetMessage(ctx context.Context, messageID, userID uuid.UUID) (*MessageResponse, error) {
	// Check access first before querying full message data.
	// This prevents leaking message existence via timing side-channels
	// and avoids expensive JOINs/decryption for unauthorized users.
	var channelID uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT channel_id FROM messages WHERE id = $1 AND deleted_at IS NULL`, messageID).Scan(&channelID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		log.Error().Err(err).Msg("Failed to look up message channel in GetMessage")
		return nil, err
	}
	if !s.channelService.CanAccessChannel(ctx, channelID, userID) {
		return nil, ErrInsufficientPerms
	}

	query := `
		SELECT m.id, m.channel_id, m.author_id, m.encrypted_content, m.reply_to_id,
		       m.link_previews, m.is_pinned, m.is_edited, m.reactions, m.created_at, m.updated_at,
		       u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		FROM messages m
		JOIN users u ON u.id = m.author_id
		WHERE m.id = $1 AND m.deleted_at IS NULL`

	var msg models.Message
	var encContent []byte
	var linkPreviewRaw []byte
	var author models.PublicUser

	err = s.db.QueryRow(ctx, query, messageID).Scan(
		&msg.ID, &msg.ChannelID, &msg.AuthorID, &encContent,
		&msg.ReplyToID, &linkPreviewRaw, &msg.IsPinned, &msg.IsEdited, &msg.Reactions, &msg.CreatedAt, &msg.UpdatedAt,
		&author.ID, &author.Username, &author.DisplayName, &author.AvatarURL, &author.Bio, &author.Status, &author.CustomStatus, &author.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		log.Error().Err(err).Msg("Failed to scan message in GetMessage")
		return nil, err
	}

	// Decrypt content
	contentStr, err := s.cipher.Decrypt(encContent, nil)
	if err != nil {
		contentErr := "[Decryption Error]"
		msg.Content = &contentErr
	} else {
		msg.Content = &contentStr
	}
	msg.LinkPreviews = messaging.DecodeLinkPreviews(linkPreviewRaw)

	response := &MessageResponse{
		Message: &msg,
		Author:  &author,
	}

	// Fetch attachments
	response.Attachments, _ = s.getMessageAttachments(ctx, messageID)

	// Fetch reactions (now from the JSONB field)
	response.Reactions = make([]ReactionSummary, 0)
	for emoji, users := range msg.Reactions {
		if len(users) > 0 {
			reacted := false
			for _, u := range users {
				if u == userID {
					reacted = true
					break
				}
			}
			response.Reactions = append(response.Reactions, ReactionSummary{
				Emoji:   emoji,
				Count:   len(users),
				Users:   users,
				Reacted: reacted,
			})
		}
	}

	// Fetch reply preview if exists
	if msg.ReplyToID != nil {
		response.ReplyTo, _ = s.getReplyPreview(ctx, *msg.ReplyToID)
	}

	return response, nil
}

// GetChannelMessages retrieves messages from a channel with pagination
func (s *Service) GetChannelMessages(ctx context.Context, channelID, userID uuid.UUID, params *GetMessagesParams) ([]*MessageResponse, error) {
	if !s.channelService.CanAccessChannel(ctx, channelID, userID) {
		return nil, ErrInsufficientPerms
	}

	limit := params.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	var query string
	var args []interface{}

	if params.Before != nil {
		query = `
			SELECT m.id, m.channel_id, m.author_id, m.encrypted_content, m.reply_to_id,
			       m.link_previews, m.is_pinned, m.is_edited, m.reactions, m.created_at, m.updated_at,
			       u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
			FROM messages m
			JOIN users u ON u.id = m.author_id
			WHERE m.channel_id = $1 AND m.deleted_at IS NULL
			  AND m.created_at < (SELECT created_at FROM messages WHERE id = $2)
			ORDER BY m.created_at DESC
			LIMIT $3`
		args = []interface{}{channelID, *params.Before, limit}
	} else if params.After != nil {
		query = `
			SELECT m.id, m.channel_id, m.author_id, m.encrypted_content, m.reply_to_id,
			       m.link_previews, m.is_pinned, m.is_edited, m.reactions, m.created_at, m.updated_at,
			       u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
			FROM messages m
			JOIN users u ON u.id = m.author_id
			WHERE m.channel_id = $1 AND m.deleted_at IS NULL
			  AND m.created_at > (SELECT created_at FROM messages WHERE id = $2)
			ORDER BY m.created_at ASC
			LIMIT $3`
		args = []interface{}{channelID, *params.After, limit}
	} else {
		query = `
			SELECT m.id, m.channel_id, m.author_id, m.encrypted_content, m.reply_to_id,
			       m.link_previews, m.is_pinned, m.is_edited, m.reactions, m.created_at, m.updated_at,
			       u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
			FROM messages m
			JOIN users u ON u.id = m.author_id
			WHERE m.channel_id = $1 AND m.deleted_at IS NULL
			ORDER BY m.created_at DESC
			LIMIT $2`
		args = []interface{}{channelID, limit}
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []*MessageResponse
	messageIDs := make([]uuid.UUID, 0)

	for rows.Next() {
		var msg models.Message
		var encContent []byte
		var linkPreviewRaw []byte
		var author models.PublicUser

		err := rows.Scan(
			&msg.ID, &msg.ChannelID, &msg.AuthorID, &encContent,
			&msg.ReplyToID, &linkPreviewRaw, &msg.IsPinned, &msg.IsEdited, &msg.Reactions, &msg.CreatedAt, &msg.UpdatedAt,
			&author.ID, &author.Username, &author.DisplayName, &author.AvatarURL, &author.Bio, &author.Status, &author.CustomStatus, &author.CreatedAt,
		)
		if err != nil {
			log.Error().Err(err).Msg("Failed to scan message in GetChannelMessages")
			return nil, err
		}

		// Decrypt content
		contentStr, err := s.cipher.Decrypt(encContent, nil)
		if err != nil {
			errStr := "[Decryption Error]"
			msg.Content = &errStr
		} else {
			msg.Content = &contentStr
		}
		msg.LinkPreviews = messaging.DecodeLinkPreviews(linkPreviewRaw)

		messages = append(messages, &MessageResponse{
			Message: &msg,
			Author:  &author,
		})
		messageIDs = append(messageIDs, msg.ID)
	}

	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("Error iterating over channel messages")
		return nil, err
	}

	// Batch fetch attachments and reply previews
	if len(messageIDs) > 0 {
		attachmentMap := s.batchGetAttachments(ctx, messageIDs)

		// Collect reply IDs for batch fetch
		replyIDs := make([]uuid.UUID, 0, len(messages))
		for _, m := range messages {
			if m.ReplyToID != nil {
				replyIDs = append(replyIDs, *m.ReplyToID)
			}
		}
		replyPreviews := s.batchGetReplyPreviews(ctx, replyIDs)

		for _, m := range messages {
			if attachments, ok := attachmentMap[m.ID]; ok {
				m.Attachments = attachments
			}

			// Populate reactions from the JSONB field
			m.Reactions = make([]ReactionSummary, 0)
			for emoji, users := range m.Message.Reactions {
				if len(users) > 0 {
					reacted := false
					for _, u := range users {
						if u == userID {
							reacted = true
							break
						}
					}
					m.Reactions = append(m.Reactions, ReactionSummary{
						Emoji:   emoji,
						Count:   len(users),
						Users:   users,
						Reacted: reacted,
					})
				}
			}

			if m.ReplyToID != nil {
				m.ReplyTo = replyPreviews[*m.ReplyToID]
			}
		}
	}

	return messages, nil
}

// UpdateMessage updates message content
func (s *Service) UpdateMessage(ctx context.Context, messageID, userID uuid.UUID, req *UpdateMessageRequest) (*MessageResponse, error) {
	// First check if user owns the message
	var authorID uuid.UUID
	err := s.db.QueryRow(ctx,
		`SELECT author_id FROM messages WHERE id = $1 AND deleted_at IS NULL`,
		messageID,
	).Scan(&authorID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, err
	}

	if authorID != userID {
		return nil, ErrNotMessageOwner
	}

	// Encrypt new content
	encryptedContent, _, err := s.cipher.Encrypt(req.Content)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt message: %w", err)
	}

	now := time.Now()
	linkPreviews := messaging.BuildLinkPreviews(ctx, req.Content)
	linkPreviewJSON := messaging.EncodeLinkPreviews(linkPreviews)

	_, err = s.db.Exec(ctx,
		`UPDATE messages SET encrypted_content = $1, link_previews = $2::jsonb, is_edited = TRUE, updated_at = $3 WHERE id = $4`,
		encryptedContent, string(linkPreviewJSON), now, messageID,
	)
	if err != nil {
		return nil, err
	}

	resp, err := s.GetMessage(ctx, messageID, userID)
	if err != nil {
		return nil, err
	}

	// Broadcast update
	s.broadcast(ctx, resp.ChannelID.String(), "MESSAGE_UPDATE", resp)

	return resp, nil
}

// DeleteMessage soft-deletes a message
func (s *Service) DeleteMessage(ctx context.Context, messageID, userID uuid.UUID, hasModPerm bool) error {
	var authorID, channelID uuid.UUID
	err := s.db.QueryRow(ctx,
		`SELECT author_id, channel_id FROM messages WHERE id = $1 AND deleted_at IS NULL`,
		messageID,
	).Scan(&authorID, &channelID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMessageNotFound
		}
		return err
	}

	// User can delete if they own the message or have mod permissions
	if authorID != userID && !hasModPerm {
		return ErrInsufficientPerms
	}

	_, err = s.db.Exec(ctx,
		`UPDATE messages SET deleted_at = $1 WHERE id = $2`,
		time.Now(), messageID,
	)
	if err != nil {
		return err
	}

	// Broadcast delete
	s.broadcast(ctx, channelID.String(), "MESSAGE_DELETE", map[string]interface{}{
		"channelId": channelID.String(),
		"messageId": messageID.String(),
	})

	return nil
}

// AddReaction adds a reaction to a message
func (s *Service) AddReaction(ctx context.Context, messageID, userID uuid.UUID, emoji string) error {
	emoji = strings.TrimSpace(emoji)
	if len(emoji) == 0 || len(emoji) > 128 {
		return ErrInvalidReaction
	}

	// Verify message exists and user can access
	var channelID uuid.UUID
	var createdAt time.Time
	err := s.db.QueryRow(ctx,
		`SELECT channel_id, created_at FROM messages WHERE id = $1 AND deleted_at IS NULL`,
		messageID,
	).Scan(&channelID, &createdAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMessageNotFound
		}
		return err
	}

	if !s.channelService.CanAccessChannel(ctx, channelID, userID) {
		return ErrInsufficientPerms
	}

	query := `
		UPDATE messages
		SET reactions = jsonb_set(
			coalesce(reactions, '{}'::jsonb),
			ARRAY[$1::text],
			(coalesce(reactions->$1, '[]'::jsonb) - $2::text) || jsonb_build_array($2::text)
		),
		updated_at = $3
		WHERE id = $4 AND created_at = $5`

	_, err = s.db.Exec(ctx, query, emoji, userID.String(), time.Now(), messageID, createdAt)
	if err != nil {
		return err
	}

	// Broadcast reaction add
	s.broadcast(ctx, channelID.String(), "REACTION_ADD", map[string]interface{}{
		"channelId": channelID.String(),
		"messageId": messageID.String(),
		"userId":    userID.String(),
		"emoji":     emoji,
	})

	return nil
}

// RemoveReaction removes a reaction from a message
func (s *Service) RemoveReaction(ctx context.Context, messageID, userID uuid.UUID, emoji string) error {
	var channelID uuid.UUID
	err := s.db.QueryRow(ctx,
		`SELECT channel_id FROM messages WHERE id = $1 AND deleted_at IS NULL`,
		messageID,
	).Scan(&channelID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMessageNotFound
		}
		return err
	}

	_, err = s.db.Exec(ctx,
		`UPDATE messages SET reactions = jsonb_set(
			reactions,
			ARRAY[$1::text],
			(reactions->$1) - $2::text
		),
		updated_at = $3
		WHERE id = $4 AND channel_id = $5`,
		emoji, userID.String(), time.Now(), messageID, channelID,
	)
	if err != nil {
		return err
	}

	// Broadcast reaction remove
	s.broadcast(ctx, channelID.String(), "REACTION_REMOVE", map[string]interface{}{
		"channelId": channelID.String(),
		"messageId": messageID.String(),
		"userId":    userID.String(),
		"emoji":     emoji,
	})

	return nil
}

// PinMessage pins/unpins a message
func (s *Service) PinMessage(ctx context.Context, messageID, userID uuid.UUID, pin bool) error {
	var channelID uuid.UUID
	err := s.db.QueryRow(ctx,
		`SELECT channel_id FROM messages WHERE id = $1 AND deleted_at IS NULL`,
		messageID,
	).Scan(&channelID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMessageNotFound
		}
		return err
	}

	if !s.channelService.CanPinMessages(ctx, channelID, userID) {
		return ErrInsufficientPerms
	}

	updatedAt := time.Now()

	_, err = s.db.Exec(ctx,
		`UPDATE messages SET is_pinned = $1, updated_at = $2 WHERE id = $3`,
		pin, updatedAt, messageID,
	)
	if err != nil {
		return err
	}

	updatedMessage, err := s.GetMessage(ctx, messageID, userID)
	if err != nil {
		return err
	}

	s.broadcast(ctx, channelID.String(), "MESSAGE_UPDATE", updatedMessage)

	return nil
}

// GetPinnedMessages gets all pinned messages in a channel
func (s *Service) GetPinnedMessages(ctx context.Context, channelID, userID uuid.UUID) ([]*MessageResponse, error) {
	if !s.channelService.CanAccessChannel(ctx, channelID, userID) {
		return nil, ErrInsufficientPerms
	}

	query := `
		SELECT m.id, m.channel_id, m.author_id, m.encrypted_content, m.reply_to_id,
		       m.link_previews, m.is_pinned, m.is_edited, m.reactions, m.created_at, m.updated_at,
		       u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		FROM messages m
		JOIN users u ON u.id = m.author_id
		WHERE m.channel_id = $1 AND m.is_pinned = true AND m.deleted_at IS NULL
		ORDER BY m.created_at DESC
		LIMIT 50`

	rows, err := s.db.Query(ctx, query, channelID)
	if err != nil {
		log.Error().Err(err).Msg("Failed to query pinned messages")
		return nil, err
	}
	defer rows.Close()

	var messages []*MessageResponse
	for rows.Next() {
		var msg models.Message
		var encContent []byte
		var linkPreviewRaw []byte
		var author models.PublicUser

		err := rows.Scan(
			&msg.ID, &msg.ChannelID, &msg.AuthorID, &encContent,
			&msg.ReplyToID, &linkPreviewRaw, &msg.IsPinned, &msg.IsEdited, &msg.Reactions, &msg.CreatedAt, &msg.UpdatedAt,
			&author.ID, &author.Username, &author.DisplayName, &author.AvatarURL, &author.Bio, &author.Status, &author.CustomStatus, &author.CreatedAt,
		)
		if err != nil {
			log.Error().Err(err).Msg("Failed to scan pinned message")
			return nil, err
		}

		contentStr, err := s.cipher.Decrypt(encContent, nil)
		if err != nil {
			errStr := "[Decryption Error]"
			msg.Content = &errStr
		} else {
			msg.Content = &contentStr
		}
		msg.LinkPreviews = messaging.DecodeLinkPreviews(linkPreviewRaw)

		messages = append(messages, &MessageResponse{
			Message: &msg,
			Author:  &author,
		})
	}

	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("Error iterating over pinned messages")
		return nil, err
	}

	return messages, nil
}

// SearchMessages searches messages in a channel
func (s *Service) SearchMessages(ctx context.Context, channelID, userID uuid.UUID, searchQuery string, limit int) ([]*MessageResponse, error) {
	searchQuery = utils.SanitizeSearchQuery(searchQuery)
	if searchQuery == "" {
		return nil, ErrMessageNotFound
	}

	if !s.channelService.CanAccessChannel(ctx, channelID, userID) {
		return nil, ErrInsufficientPerms
	}

	if limit <= 0 || limit > 50 {
		limit = 25
	}

	// Note: Searching encrypted content is complex. This is a simplified approach.
	// In production, we might use a separate search index with encrypted tokens.
	// For now, we'll fetch recent messages and filter client-side or use metadata.

	// This query searches by author username as a simple example
	query := `
		SELECT m.id, m.channel_id, m.author_id, m.encrypted_content, m.reply_to_id,
		       m.link_previews, m.is_pinned, m.created_at, m.updated_at, m.is_edited,
		       u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		FROM messages m
		JOIN users u ON u.id = m.author_id
		WHERE m.channel_id = $1 AND m.deleted_at IS NULL
		  AND u.username ILIKE '%' || $2 || '%'
		ORDER BY m.created_at DESC
		LIMIT $3`

	rows, err := s.db.Query(ctx, query, channelID, searchQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []*MessageResponse
	for rows.Next() {
		var msg models.Message
		var encContent []byte
		var linkPreviewRaw []byte
		var author models.PublicUser

		err := rows.Scan(
			&msg.ID, &msg.ChannelID, &msg.AuthorID, &encContent,
			&msg.ReplyToID, &linkPreviewRaw, &msg.IsPinned, &msg.CreatedAt, &msg.UpdatedAt, &msg.IsEdited,
			&author.ID, &author.Username, &author.DisplayName, &author.AvatarURL, &author.Bio, &author.Status, &author.CustomStatus, &author.CreatedAt,
		)
		if err != nil {
			return nil, err
		}

		contentStr, err := s.cipher.Decrypt(encContent, nil)
		if err != nil {
			errStr := "[Decryption Error]"
			msg.Content = &errStr
		} else {
			msg.Content = &contentStr
		}
		msg.LinkPreviews = messaging.DecodeLinkPreviews(linkPreviewRaw)

		messages = append(messages, &MessageResponse{
			Message: &msg,
			Author:  &author,
		})
	}

	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("Error iterating over search results")
		return nil, err
	}

	return messages, nil
}

// Helper functions
func (s *Service) getMessageAttachments(ctx context.Context, messageID uuid.UUID) ([]models.MessageAttachment, error) {
	query := `
		SELECT id, message_id, uploader_id, filename, file_url, file_size, content_type, thumbnail_url, width, height, created_at
		FROM message_attachments
		WHERE message_id = $1`

	rows, err := s.db.Query(ctx, query, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attachments []models.MessageAttachment
	for rows.Next() {
		var a models.MessageAttachment
		err := rows.Scan(&a.ID, &a.MessageID, &a.UploaderID, &a.Filename, &a.FileURL,
			&a.FileSize, &a.ContentType, &a.ThumbnailURL, &a.Width, &a.Height, &a.CreatedAt)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, a)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating over message attachments")
		return nil, err
	}

	return attachments, nil
}

func (s *Service) getReplyPreview(ctx context.Context, messageID uuid.UUID) (*MessageReplyPreview, error) {
	query := `
		SELECT m.id, m.encrypted_content, m.author_id,
		       u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		FROM messages m
		JOIN users u ON u.id = m.author_id
		WHERE m.id = $1`

	var preview MessageReplyPreview
	var encContent []byte
	var author models.PublicUser

	err := s.db.QueryRow(ctx, query, messageID).Scan(
		&preview.ID, &encContent, &preview.AuthorID,
		&author.ID, &author.Username, &author.DisplayName, &author.AvatarURL, &author.Bio, &author.Status, &author.CustomStatus, &author.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	contentStr, err := s.cipher.Decrypt(encContent, nil)
	if err != nil {
		preview.Content = "[Decryption Error]"
	} else {
		if len(contentStr) > 100 {
			contentStr = contentStr[:100] + "..."
		}
		preview.Content = contentStr
	}
	preview.Author = &author

	return &preview, nil
}

func (s *Service) CanManageMessages(ctx context.Context, channelID, userID uuid.UUID) bool {
	return s.channelService.CanManageMessages(ctx, channelID, userID)
}

func (s *Service) CanPinMessages(ctx context.Context, channelID, userID uuid.UUID) bool {
	return s.channelService.CanPinMessages(ctx, channelID, userID)
}

func (s *Service) batchGetReplyPreviews(ctx context.Context, messageIDs []uuid.UUID) map[uuid.UUID]*MessageReplyPreview {
	result := make(map[uuid.UUID]*MessageReplyPreview)
	if len(messageIDs) == 0 {
		return result
	}

	query := `
		SELECT m.id, m.encrypted_content, m.author_id,
		       u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		FROM messages m
		JOIN users u ON u.id = m.author_id
		WHERE m.id = ANY($1)`

	rows, err := s.db.Query(ctx, query, messageIDs)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var preview MessageReplyPreview
		var encContent []byte
		var author models.PublicUser

		err := rows.Scan(
			&preview.ID, &encContent, &preview.AuthorID,
			&author.ID, &author.Username, &author.DisplayName, &author.AvatarURL, &author.Bio, &author.Status, &author.CustomStatus, &author.CreatedAt,
		)
		if err != nil {
			continue
		}

		contentStr, err := s.cipher.Decrypt(encContent, nil)
		if err != nil {
			preview.Content = "[Decryption Error]"
		} else {
			if len(contentStr) > 100 {
				contentStr = contentStr[:100] + "..."
			}
			preview.Content = contentStr
		}
		preview.Author = &author

		result[preview.ID] = &preview
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating over batch reply previews")
	}

	return result
}

func (s *Service) batchGetAttachments(ctx context.Context, messageIDs []uuid.UUID) map[uuid.UUID][]models.MessageAttachment {
	result := make(map[uuid.UUID][]models.MessageAttachment)

	query := `
		SELECT id, message_id, uploader_id, filename, file_url, file_size, content_type, thumbnail_url, width, height, created_at
		FROM message_attachments
		WHERE message_id = ANY($1)`

	rows, err := s.db.Query(ctx, query, messageIDs)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var a models.MessageAttachment
		err := rows.Scan(&a.ID, &a.MessageID, &a.UploaderID, &a.Filename, &a.FileURL,
			&a.FileSize, &a.ContentType, &a.ThumbnailURL, &a.Width, &a.Height, &a.CreatedAt)
		if err != nil {
			continue
		}
		if a.MessageID != nil {
			result[*a.MessageID] = append(result[*a.MessageID], a)
		}
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating over batch attachments")
	}

	return result
}

// Typing indicator methods using Redis
func (s *Service) SetTyping(ctx context.Context, channelID, userID uuid.UUID) error {
	key := fmt.Sprintf("typing:%s", channelID.String())
	member := userID.String()

	// Add to sorted set with current timestamp as score
	s.redis.ZAdd(ctx, key, redis.Z{
		Score:  float64(time.Now().Unix()),
		Member: member,
	})
	s.redis.Expire(ctx, key, 10*time.Second)

	// Publish typing event
	event := map[string]interface{}{
		"channelId": channelID.String(),
		"userId":    userID.String(),
		"typing":    true,
	}
	eventJSON, _ := json.Marshal(event)
	s.redis.Publish(ctx, fmt.Sprintf("channel:%s:typing", channelID.String()), eventJSON)

	return nil
}

func (s *Service) GetTypingUsers(ctx context.Context, channelID uuid.UUID) ([]uuid.UUID, error) {
	key := fmt.Sprintf("typing:%s", channelID.String())

	// Get users who typed in the last 5 seconds
	cutoff := float64(time.Now().Add(-5 * time.Second).Unix())

	members, err := s.redis.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: fmt.Sprintf("%f", cutoff),
		Max: "+inf",
	}).Result()
	if err != nil {
		return nil, err
	}

	var users []uuid.UUID
	for _, m := range members {
		if id, err := uuid.Parse(m); err == nil {
			users = append(users, id)
		}
	}

	return users, nil
}
