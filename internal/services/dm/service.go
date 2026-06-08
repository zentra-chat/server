package dm

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
	"github.com/zentra/server/pkg/encryption"
)

var (
	ErrConversationNotFound = errors.New("conversation not found")
	ErrNotParticipant       = errors.New("not a conversation participant")
	ErrMessageNotFound      = errors.New("message not found")
	ErrNotMessageOwner      = errors.New("not message owner")
	ErrBlocked              = errors.New("user is blocked")
	ErrInvalidAttachment    = errors.New("invalid attachment")
	ErrInvalidReaction      = errors.New("invalid reaction")
)

type Service struct {
	db                  *pgxpool.Pool
	redis               *redis.Client
	userService         UserServiceInterface
	notificationService *notification.Service
	cipher              messaging.ContentCipher
	masterKey           []byte
}

type UserServiceInterface interface {
	GetPublicUser(ctx context.Context, id uuid.UUID) (*models.PublicUser, error)
	IsBlocked(ctx context.Context, blockerID, blockedID uuid.UUID) (bool, error)
}

func NewService(db *pgxpool.Pool, redis *redis.Client, encryptionKey []byte, userService UserServiceInterface) *Service {
	return &Service{
		db:          db,
		redis:       redis,
		userService: userService,
		cipher:      messaging.NewDMCipher(),
		masterKey:   encryptionKey,
	}
}

func (s *Service) conversationDEK(ctx context.Context, conversationID uuid.UUID) ([]byte, error) {
	var wrapped []byte
	err := s.db.QueryRow(ctx,
		`SELECT encrypted_dek FROM dm_conversations WHERE id = $1`,
		conversationID,
	).Scan(&wrapped)
	if err != nil {
		return nil, fmt.Errorf("load conversation DEK: %w", err)
	}
	return encryption.UnwrapKey(wrapped, s.masterKey)
}

// SetNotificationService wires the notification service after construction
// (both services depend on the wsHub which is created after them).
func (s *Service) SetNotificationService(ns *notification.Service) {
	s.notificationService = ns
}

type CreateConversationRequest struct {
	UserID uuid.UUID `json:"userId" validate:"required"`
}

type SendMessageRequest struct {
	Content     string      `json:"content" validate:"required_without=Attachments,max=4000"`
	ReplyToID   *uuid.UUID  `json:"replyToId,omitempty"`
	Attachments []uuid.UUID `json:"attachments,omitempty" validate:"max=10"`
}

type UpdateMessageRequest struct {
	Content string `json:"content" validate:"required,max=4000"`
}

type DMMessageResponse struct {
	ID             uuid.UUID                  `json:"id"`
	ConversationID uuid.UUID                  `json:"conversationId"`
	SenderID       uuid.UUID                  `json:"senderId"`
	Content        string                     `json:"content"`
	IsEdited       bool                       `json:"isEdited"`
	Reactions      []models.ReactionCount     `json:"reactions,omitempty"`
	Attachments    []models.MessageAttachment `json:"attachments,omitempty"`
	LinkPreviews   []models.LinkPreview       `json:"linkPreviews,omitempty"`
	ReplyTo        *DMReplyPreview            `json:"replyTo,omitempty"`
	CreatedAt      time.Time                  `json:"createdAt"`
	UpdatedAt      time.Time                  `json:"updatedAt"`
	Sender         *models.PublicUser         `json:"sender,omitempty"`
}

type DMReplyPreview struct {
	ID       uuid.UUID          `json:"id"`
	Content  string             `json:"content"`
	SenderID uuid.UUID          `json:"senderId"`
	Sender   *models.PublicUser `json:"sender"`
}

type DMConversationResponse struct {
	ID           uuid.UUID           `json:"id"`
	Participants []models.PublicUser `json:"participants"`
	LastMessage  *DMMessageResponse  `json:"lastMessage,omitempty"`
	UnreadCount  int                 `json:"unreadCount"`
	CreatedAt    time.Time           `json:"createdAt"`
	UpdatedAt    time.Time           `json:"updatedAt"`
}

type GetMessagesParams struct {
	Before *uuid.UUID
	After  *uuid.UUID
	Limit  int
}

func (s *Service) broadcast(ctx context.Context, conversationID string, eventType string, data interface{}) {
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
		ChannelID: conversationID,
		Event:     event,
	}

	jsonData, err := json.Marshal(broadcast)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal DM broadcast")
		return
	}

	if err := s.redis.Publish(ctx, "websocket:broadcast", jsonData).Err(); err != nil {
		log.Error().Err(err).Msg("Failed to publish DM broadcast")
	}
}

func (s *Service) CanAccessConversation(ctx context.Context, conversationID, userID uuid.UUID) bool {
	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM dm_participants WHERE conversation_id = $1 AND user_id = $2)`,
		conversationID, userID,
	).Scan(&exists)
	return err == nil && exists
}

func (s *Service) CreateOrGetConversation(ctx context.Context, userID, otherUserID uuid.UUID) (*DMConversationResponse, error) {
	if userID == otherUserID {
		return nil, fmt.Errorf("cannot DM yourself")
	}

	if blocked, err := s.userService.IsBlocked(ctx, userID, otherUserID); err != nil {
		return nil, err
	} else if blocked {
		return nil, ErrBlocked
	}

	if blocked, err := s.userService.IsBlocked(ctx, otherUserID, userID); err != nil {
		return nil, err
	} else if blocked {
		return nil, ErrBlocked
	}

	var convo models.DMConversation
	err := s.db.QueryRow(ctx,
		`SELECT c.id, c.created_at, c.updated_at
		 FROM dm_conversations c
		 JOIN dm_participants p1 ON p1.conversation_id = c.id AND p1.user_id = $1
		 JOIN dm_participants p2 ON p2.conversation_id = c.id AND p2.user_id = $2
		 LIMIT 1`,
		userID, otherUserID,
	).Scan(&convo.ID, &convo.CreatedAt, &convo.UpdatedAt)
	if err == nil {
		return s.buildConversationResponse(ctx, convo, userID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	dek, err := encryption.GenerateDEK()
	if err != nil {
		return nil, err
	}
	wrappedDEK, err := encryption.WrapKey(dek, s.masterKey)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	convo = models.DMConversation{ID: uuid.New(), CreatedAt: now, UpdatedAt: now}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx,
		`INSERT INTO dm_conversations (id, encrypted_dek, created_at, updated_at) VALUES ($1, $2, $3, $3)`,
		convo.ID, wrappedDEK, now,
	)
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO dm_participants (conversation_id, user_id, last_read_at) VALUES ($1, $2, $3)`+
			`ON CONFLICT (conversation_id, user_id) DO NOTHING`,
		convo.ID, userID, now,
	)
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO dm_participants (conversation_id, user_id, last_read_at) VALUES ($1, $2, NULL)`+
			`ON CONFLICT (conversation_id, user_id) DO NOTHING`,
		convo.ID, otherUserID,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return s.buildConversationResponse(ctx, convo, userID)
}

func (s *Service) ListConversations(ctx context.Context, userID uuid.UUID) ([]*DMConversationResponse, error) {
	rows, err := s.db.Query(ctx,
		`SELECT c.id, c.created_at, c.updated_at
		 FROM dm_conversations c
		 JOIN dm_participants p ON p.conversation_id = c.id
		 WHERE p.user_id = $1
		 ORDER BY c.updated_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var responses []*DMConversationResponse
	for rows.Next() {
		var convo models.DMConversation
		if err := rows.Scan(&convo.ID, &convo.CreatedAt, &convo.UpdatedAt); err != nil {
			return nil, err
		}
		resp, err := s.buildConversationResponse(ctx, convo, userID)
		if err != nil {
			return nil, err
		}
		responses = append(responses, resp)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating over conversations")
		return nil, err
	}

	return responses, nil
}

func (s *Service) GetConversation(ctx context.Context, conversationID, userID uuid.UUID) (*DMConversationResponse, error) {
	if !s.CanAccessConversation(ctx, conversationID, userID) {
		return nil, ErrNotParticipant
	}

	var convo models.DMConversation
	err := s.db.QueryRow(ctx,
		`SELECT id, created_at, updated_at FROM dm_conversations WHERE id = $1`,
		conversationID,
	).Scan(&convo.ID, &convo.CreatedAt, &convo.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrConversationNotFound
		}
		return nil, err
	}

	return s.buildConversationResponse(ctx, convo, userID)
}

func (s *Service) GetMessages(ctx context.Context, conversationID, userID uuid.UUID, params *GetMessagesParams) ([]*DMMessageResponse, error) {
	if !s.CanAccessConversation(ctx, conversationID, userID) {
		return nil, ErrNotParticipant
	}

	limit := params.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	var query string
	var args []interface{}

	if params.Before != nil {
		query = `
			SELECT m.id, m.conversation_id, m.sender_id, m.encrypted_content, m.nonce, m.reply_to_id, m.is_edited, m.reactions, m.link_previews, m.created_at, m.updated_at,
			       u.id, u.username, u.display_name, u.avatar_url, u.banner_url, u.bio, u.status, u.custom_status, u.created_at
			FROM direct_messages m
			JOIN users u ON u.id = m.sender_id
			WHERE m.conversation_id = $1 AND m.deleted_at IS NULL
			  AND m.created_at < (SELECT created_at FROM direct_messages WHERE id = $2)
			ORDER BY m.created_at DESC
			LIMIT $3`
		args = []interface{}{conversationID, *params.Before, limit}
	} else if params.After != nil {
		query = `
			SELECT m.id, m.conversation_id, m.sender_id, m.encrypted_content, m.nonce, m.reply_to_id, m.is_edited, m.reactions, m.link_previews, m.created_at, m.updated_at,
			       u.id, u.username, u.display_name, u.avatar_url, u.banner_url, u.bio, u.status, u.custom_status, u.created_at
			FROM direct_messages m
			JOIN users u ON u.id = m.sender_id
			WHERE m.conversation_id = $1 AND m.deleted_at IS NULL
			  AND m.created_at > (SELECT created_at FROM direct_messages WHERE id = $2)
			ORDER BY m.created_at ASC
			LIMIT $3`
		args = []interface{}{conversationID, *params.After, limit}
	} else {
		query = `
			SELECT m.id, m.conversation_id, m.sender_id, m.encrypted_content, m.nonce, m.reply_to_id, m.is_edited, m.reactions, m.link_previews, m.created_at, m.updated_at,
			       u.id, u.username, u.display_name, u.avatar_url, u.banner_url, u.bio, u.status, u.custom_status, u.created_at
			FROM direct_messages m
			JOIN users u ON u.id = m.sender_id
			WHERE m.conversation_id = $1 AND m.deleted_at IS NULL
			ORDER BY m.created_at DESC
			LIMIT $2`
		args = []interface{}{conversationID, limit}
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	convKey, err := s.conversationDEK(ctx, conversationID)
	if err != nil {
		return nil, err
	}

	var messages []*DMMessageResponse
	messageIDs := make([]uuid.UUID, 0)
	for rows.Next() {
		var msg models.DirectMessage
		var nonce []byte
		var linkPreviewRaw []byte
		var sender models.PublicUser

		if err := rows.Scan(
			&msg.ID, &msg.ConversationID, &msg.SenderID, &msg.EncryptedContent, &nonce,
			&msg.ReplyToID, &msg.IsEdited, &msg.Reactions, &linkPreviewRaw, &msg.CreatedAt, &msg.UpdatedAt,
			&sender.ID, &sender.Username, &sender.DisplayName, &sender.AvatarURL, &sender.BannerURL, &sender.Bio, &sender.Status, &sender.CustomStatus, &sender.CreatedAt,
		); err != nil {
			return nil, err
		}
		msg.LinkPreviews = messaging.DecodeLinkPreviews(linkPreviewRaw)

		content, err := s.cipher.Decrypt(msg.EncryptedContent, nonce, convKey)
		if err != nil {
			content = "[Decryption Error]"
		}

		response := &DMMessageResponse{
			ID:             msg.ID,
			ConversationID: msg.ConversationID,
			SenderID:       msg.SenderID,
			Content:        content,
			IsEdited:       msg.IsEdited,
			Reactions:      s.buildReactions(msg.Reactions, userID),
			LinkPreviews:   msg.LinkPreviews,
			CreatedAt:      msg.CreatedAt,
			UpdatedAt:      msg.UpdatedAt,
			Sender:         &sender,
		}
		if msg.ReplyToID != nil {
			response.ReplyTo, _ = s.getReplyPreview(ctx, *msg.ReplyToID)
		}
		messages = append(messages, response)
		messageIDs = append(messageIDs, msg.ID)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating over DM messages")
		return nil, err
	}

	if len(messageIDs) > 0 {
		attachmentMap := s.batchGetDmAttachments(ctx, messageIDs)
		for _, message := range messages {
			if attachments, ok := attachmentMap[message.ID]; ok {
				message.Attachments = attachments
			}
		}
	}

	return messages, nil
}

func (s *Service) SendMessage(ctx context.Context, conversationID, userID uuid.UUID, req *SendMessageRequest) (*DMMessageResponse, error) {
	if !s.CanAccessConversation(ctx, conversationID, userID) {
		return nil, ErrNotParticipant
	}

	linkPreviews := messaging.BuildLinkPreviews(ctx, req.Content)
	linkPreviewJSON := messaging.EncodeLinkPreviews(linkPreviews)

	sendKey, err := s.conversationDEK(ctx, conversationID)
	if err != nil {
		return nil, err
	}

	ciphertext, nonce, err := s.cipher.Encrypt(req.Content, sendKey)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	messageID := uuid.New()

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if req.ReplyToID != nil {
		var exists bool
		err = tx.QueryRow(ctx,
			`SELECT EXISTS(
				SELECT 1 FROM direct_messages
				WHERE id = $1 AND conversation_id = $2 AND deleted_at IS NULL
			)`,
			*req.ReplyToID, conversationID,
		).Scan(&exists)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrMessageNotFound
		}
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO direct_messages (id, conversation_id, sender_id, encrypted_content, nonce, reply_to_id, link_previews, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $8)`,
		messageID, conversationID, userID, ciphertext, nonce, req.ReplyToID, string(linkPreviewJSON), now,
	)
	if err != nil {
		return nil, err
	}

	if len(req.Attachments) > 0 {
		for _, attachmentID := range req.Attachments {
			tag, err := tx.Exec(ctx,
				`UPDATE message_attachments
				 SET dm_message_id = $1
				 WHERE id = $2
				   AND uploader_id = $3
				   AND dm_message_id IS NULL
				   AND dm_conversation_id = $4`,
				messageID, attachmentID, userID, conversationID,
			)
			if err != nil {
				return nil, err
			}
			if tag.RowsAffected() == 0 {
				return nil, ErrInvalidAttachment
			}
		}
	}

	_, err = tx.Exec(ctx,
		`UPDATE dm_conversations SET updated_at = $2 WHERE id = $1`,
		conversationID, now,
	)
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx,
		`UPDATE dm_participants SET last_read_at = $3 WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, userID, now,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	resp, err := s.GetMessage(ctx, messageID, userID)
	if err != nil {
		return nil, err
	}

	s.broadcast(ctx, conversationID.String(), "DM_MESSAGE_CREATE", resp)

	// Dispatch DM notification to other participants.
	if s.notificationService != nil {
		senderName := ""
		if resp.Sender != nil {
			if resp.Sender.DisplayName != nil && *resp.Sender.DisplayName != "" {
				senderName = *resp.Sender.DisplayName
			} else {
				senderName = resp.Sender.Username
			}
		}
		go s.notificationService.ProcessDMNotification(notification.DMNotificationContext{
			ConversationID: conversationID,
			MessageID:      messageID,
			SenderID:       userID,
			SenderName:     senderName,
			Content:        req.Content,
		})
	}

	return resp, nil
}

func (s *Service) GetMessage(ctx context.Context, messageID, userID uuid.UUID) (*DMMessageResponse, error) {
	var msg models.DirectMessage
	var nonce []byte
	var linkPreviewRaw []byte
	var sender models.PublicUser

	err := s.db.QueryRow(ctx,
		`SELECT m.id, m.conversation_id, m.sender_id, m.encrypted_content, m.nonce, m.reply_to_id, m.is_edited, m.reactions, m.link_previews, m.created_at, m.updated_at,
		        u.id, u.username, u.display_name, u.avatar_url, u.banner_url, u.bio, u.status, u.custom_status, u.created_at
		 FROM direct_messages m
		 JOIN users u ON u.id = m.sender_id
		 WHERE m.id = $1 AND m.deleted_at IS NULL`,
		messageID,
	).Scan(
		&msg.ID, &msg.ConversationID, &msg.SenderID, &msg.EncryptedContent, &nonce, &msg.ReplyToID, &msg.IsEdited, &msg.Reactions, &linkPreviewRaw, &msg.CreatedAt, &msg.UpdatedAt,
		&sender.ID, &sender.Username, &sender.DisplayName, &sender.AvatarURL, &sender.BannerURL, &sender.Bio, &sender.Status, &sender.CustomStatus, &sender.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, err
	}

	if !s.CanAccessConversation(ctx, msg.ConversationID, userID) {
		return nil, ErrNotParticipant
	}

	getKey, err := s.conversationDEK(ctx, msg.ConversationID)
	if err != nil {
		return nil, err
	}

	content, err := s.cipher.Decrypt(msg.EncryptedContent, nonce, getKey)
	if err != nil {
		content = "[Decryption Error]"
	}
	msg.LinkPreviews = messaging.DecodeLinkPreviews(linkPreviewRaw)

	attachments, _ := s.getDmMessageAttachments(ctx, msg.ID)

	response := &DMMessageResponse{
		ID:             msg.ID,
		ConversationID: msg.ConversationID,
		SenderID:       msg.SenderID,
		Content:        content,
		IsEdited:       msg.IsEdited,
		Reactions:      s.buildReactions(msg.Reactions, userID),
		Attachments:    attachments,
		LinkPreviews:   msg.LinkPreviews,
		CreatedAt:      msg.CreatedAt,
		UpdatedAt:      msg.UpdatedAt,
		Sender:         &sender,
	}
	if msg.ReplyToID != nil {
		response.ReplyTo, _ = s.getReplyPreview(ctx, *msg.ReplyToID)
	}

	return response, nil
}

func (s *Service) UpdateMessage(ctx context.Context, messageID, userID uuid.UUID, req *UpdateMessageRequest) (*DMMessageResponse, error) {
	var senderID uuid.UUID
	var conversationID uuid.UUID

	err := s.db.QueryRow(ctx,
		`SELECT sender_id, conversation_id FROM direct_messages WHERE id = $1 AND deleted_at IS NULL`,
		messageID,
	).Scan(&senderID, &conversationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMessageNotFound
		}
		return nil, err
	}

	if senderID != userID {
		return nil, ErrNotMessageOwner
	}

	linkPreviews := messaging.BuildLinkPreviews(ctx, req.Content)
	linkPreviewJSON := messaging.EncodeLinkPreviews(linkPreviews)

	updateKey, err := s.conversationDEK(ctx, conversationID)
	if err != nil {
		return nil, err
	}

	ciphertext, nonce, err := s.cipher.Encrypt(req.Content, updateKey)
	if err != nil {
		return nil, err
	}

	_, err = s.db.Exec(ctx,
		`UPDATE direct_messages SET encrypted_content = $1, nonce = $2, link_previews = $3::jsonb, is_edited = TRUE, updated_at = $4 WHERE id = $5`,
		ciphertext, nonce, string(linkPreviewJSON), time.Now(), messageID,
	)
	if err != nil {
		return nil, err
	}

	resp, err := s.GetMessage(ctx, messageID, userID)
	if err != nil {
		return nil, err
	}

	s.broadcast(ctx, conversationID.String(), "DM_MESSAGE_UPDATE", resp)

	return resp, nil
}

func (s *Service) DeleteMessage(ctx context.Context, messageID, userID uuid.UUID) error {
	var senderID uuid.UUID
	var conversationID uuid.UUID

	err := s.db.QueryRow(ctx,
		`SELECT sender_id, conversation_id FROM direct_messages WHERE id = $1 AND deleted_at IS NULL`,
		messageID,
	).Scan(&senderID, &conversationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMessageNotFound
		}
		return err
	}

	if senderID != userID {
		return ErrNotMessageOwner
	}

	_, err = s.db.Exec(ctx,
		`UPDATE direct_messages SET deleted_at = $1 WHERE id = $2`,
		time.Now(), messageID,
	)
	if err != nil {
		return err
	}

	s.broadcast(ctx, conversationID.String(), "DM_MESSAGE_DELETE", map[string]interface{}{
		"conversationId": conversationID.String(),
		"messageId":      messageID.String(),
	})

	return nil
}

func (s *Service) AddReaction(ctx context.Context, messageID, userID uuid.UUID, emoji string) error {
	emoji = strings.TrimSpace(emoji)
	if len(emoji) == 0 || len(emoji) > 128 {
		return ErrInvalidReaction
	}

	var conversationID uuid.UUID
	err := s.db.QueryRow(ctx,
		`SELECT conversation_id FROM direct_messages WHERE id = $1 AND deleted_at IS NULL`,
		messageID,
	).Scan(&conversationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMessageNotFound
		}
		return err
	}

	if !s.CanAccessConversation(ctx, conversationID, userID) {
		return ErrNotParticipant
	}

	query := `
		UPDATE direct_messages
		SET reactions = jsonb_set(
			coalesce(reactions, '{}'::jsonb),
			ARRAY[$1::text],
			(coalesce(reactions->$1, '[]'::jsonb) - $2::text) || jsonb_build_array($2::text)
		),
		updated_at = $3
		WHERE id = $4 AND deleted_at IS NULL`

	_, err = s.db.Exec(ctx, query, emoji, userID.String(), time.Now(), messageID)
	if err != nil {
		return err
	}

	s.broadcast(ctx, conversationID.String(), "DM_REACTION_ADD", map[string]interface{}{
		"conversationId": conversationID.String(),
		"messageId":      messageID.String(),
		"userId":         userID.String(),
		"emoji":          emoji,
	})

	return nil
}

func (s *Service) RemoveReaction(ctx context.Context, messageID, userID uuid.UUID, emoji string) error {
	var conversationID uuid.UUID
	err := s.db.QueryRow(ctx,
		`SELECT conversation_id FROM direct_messages WHERE id = $1 AND deleted_at IS NULL`,
		messageID,
	).Scan(&conversationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMessageNotFound
		}
		return err
	}

	if !s.CanAccessConversation(ctx, conversationID, userID) {
		return ErrNotParticipant
	}

	query := `
		UPDATE direct_messages
		SET reactions = jsonb_set(
			reactions,
			ARRAY[$1::text],
			(reactions->$1) - $2::text
		),
		updated_at = $3
		WHERE id = $4 AND deleted_at IS NULL`

	_, err = s.db.Exec(ctx, query, emoji, userID.String(), time.Now(), messageID)
	if err != nil {
		return err
	}

	s.broadcast(ctx, conversationID.String(), "DM_REACTION_REMOVE", map[string]interface{}{
		"conversationId": conversationID.String(),
		"messageId":      messageID.String(),
		"userId":         userID.String(),
		"emoji":          emoji,
	})

	return nil
}

func (s *Service) MarkRead(ctx context.Context, conversationID, userID uuid.UUID) error {
	if !s.CanAccessConversation(ctx, conversationID, userID) {
		return ErrNotParticipant
	}

	_, err := s.db.Exec(ctx,
		`UPDATE dm_participants SET last_read_at = $3 WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, userID, time.Now(),
	)
	return err
}

func (s *Service) buildConversationResponse(ctx context.Context, convo models.DMConversation, userID uuid.UUID) (*DMConversationResponse, error) {
	participants, err := s.getParticipants(ctx, convo.ID)
	if err != nil {
		return nil, err
	}

	lastMessage, err := s.getLastMessage(ctx, convo.ID, participants, userID)
	if err != nil {
		return nil, err
	}

	unreadCount, err := s.getUnreadCount(ctx, convo.ID, userID)
	if err != nil {
		return nil, err
	}

	return &DMConversationResponse{
		ID:           convo.ID,
		Participants: participants,
		LastMessage:  lastMessage,
		UnreadCount:  unreadCount,
		CreatedAt:    convo.CreatedAt,
		UpdatedAt:    convo.UpdatedAt,
	}, nil
}

func (s *Service) getParticipants(ctx context.Context, conversationID uuid.UUID) ([]models.PublicUser, error) {
	rows, err := s.db.Query(ctx,
		`SELECT u.id, u.username, u.display_name, u.avatar_url, u.banner_url, u.bio, u.status, u.custom_status, u.created_at
		 FROM dm_participants p
		 JOIN users u ON u.id = p.user_id
		 WHERE p.conversation_id = $1 AND u.deleted_at IS NULL`,
		conversationID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var participants []models.PublicUser
	for rows.Next() {
		var user models.PublicUser
		if err := rows.Scan(&user.ID, &user.Username, &user.DisplayName, &user.AvatarURL, &user.BannerURL, &user.Bio, &user.Status, &user.CustomStatus, &user.CreatedAt); err != nil {
			return nil, err
		}
		participants = append(participants, user)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating over participants")
		return nil, err
	}

	return participants, nil
}

func (s *Service) getLastMessage(ctx context.Context, conversationID uuid.UUID, participants []models.PublicUser, userID uuid.UUID) (*DMMessageResponse, error) {
	var msg models.DirectMessage
	var nonce []byte
	var linkPreviewRaw []byte

	err := s.db.QueryRow(ctx,
		`SELECT id, conversation_id, sender_id, encrypted_content, nonce, reply_to_id, is_edited, reactions, link_previews, created_at, updated_at
		 FROM direct_messages
		 WHERE conversation_id = $1 AND deleted_at IS NULL
		 ORDER BY created_at DESC
		 LIMIT 1`,
		conversationID,
	).Scan(&msg.ID, &msg.ConversationID, &msg.SenderID, &msg.EncryptedContent, &nonce, &msg.ReplyToID, &msg.IsEdited, &msg.Reactions, &linkPreviewRaw, &msg.CreatedAt, &msg.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	lastKey, err := s.conversationDEK(ctx, conversationID)
	if err != nil {
		return nil, err
	}

	content, err := s.cipher.Decrypt(msg.EncryptedContent, nonce, lastKey)
	if err != nil {
		content = "[Decryption Error]"
	}
	msg.LinkPreviews = messaging.DecodeLinkPreviews(linkPreviewRaw)

	var sender *models.PublicUser
	for i := range participants {
		if participants[i].ID == msg.SenderID {
			sender = &participants[i]
			break
		}
	}
	if sender == nil {
		u, err := s.userService.GetPublicUser(ctx, msg.SenderID)
		if err == nil {
			sender = u
		}
	}

	attachments, _ := s.getDmMessageAttachments(ctx, msg.ID)

	response := &DMMessageResponse{
		ID:             msg.ID,
		ConversationID: msg.ConversationID,
		SenderID:       msg.SenderID,
		Content:        content,
		IsEdited:       msg.IsEdited,
		Reactions:      s.buildReactions(msg.Reactions, userID),
		Attachments:    attachments,
		LinkPreviews:   msg.LinkPreviews,
		CreatedAt:      msg.CreatedAt,
		UpdatedAt:      msg.UpdatedAt,
		Sender:         sender,
	}
	if msg.ReplyToID != nil {
		response.ReplyTo, _ = s.getReplyPreview(ctx, *msg.ReplyToID)
	}

	return response, nil
}

func (s *Service) getUnreadCount(ctx context.Context, conversationID, userID uuid.UUID) (int, error) {
	var count int
	var lastRead *time.Time

	if err := s.db.QueryRow(ctx,
		`SELECT last_read_at FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, userID,
	).Scan(&lastRead); err != nil {
		log.Warn().Err(err).Msg("Failed to get last read timestamp for unread count")
	}

	if lastRead == nil {
		lastReadTime := time.Unix(0, 0)
		lastRead = &lastReadTime
	}

	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM direct_messages
		 WHERE conversation_id = $1 AND deleted_at IS NULL
		   AND created_at > $2 AND sender_id <> $3`,
		conversationID, *lastRead, userID,
	).Scan(&count)
	if err != nil {
		return 0, err
	}

	return count, nil
}

func (s *Service) buildReactions(reactions map[string][]uuid.UUID, userID uuid.UUID) []models.ReactionCount {
	if len(reactions) == 0 {
		return nil
	}

	result := make([]models.ReactionCount, 0)
	for emoji, users := range reactions {
		if len(users) == 0 {
			continue
		}
		reacted := false
		for _, u := range users {
			if u == userID {
				reacted = true
				break
			}
		}
		result = append(result, models.ReactionCount{
			Emoji:   emoji,
			Count:   len(users),
			Reacted: reacted,
		})
	}

	return result
}

func (s *Service) getReplyPreview(ctx context.Context, messageID uuid.UUID) (*DMReplyPreview, error) {
	query := `
		SELECT m.id, m.conversation_id, m.sender_id, m.encrypted_content, m.nonce,
		       u.id, u.username, u.display_name, u.avatar_url, u.banner_url, u.bio, u.status, u.custom_status, u.created_at
		FROM direct_messages m
		JOIN users u ON u.id = m.sender_id
		WHERE m.id = $1 AND m.deleted_at IS NULL`

	var preview DMReplyPreview
	var convID uuid.UUID
	var encContent []byte
	var nonce []byte
	var sender models.PublicUser

	err := s.db.QueryRow(ctx, query, messageID).Scan(
		&preview.ID, &convID, &preview.SenderID, &encContent, &nonce,
		&sender.ID, &sender.Username, &sender.DisplayName, &sender.AvatarURL, &sender.BannerURL, &sender.Bio, &sender.Status, &sender.CustomStatus, &sender.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	replyKey, err := s.conversationDEK(ctx, convID)
	if err != nil {
		return nil, err
	}

	content, err := s.cipher.Decrypt(encContent, nonce, replyKey)
	if err != nil {
		content = "[Decryption Error]"
	} else if len(content) > 100 {
		content = content[:100] + "..."
	}

	preview.Content = content
	preview.Sender = &sender

	return &preview, nil
}

func (s *Service) getDmMessageAttachments(ctx context.Context, messageID uuid.UUID) ([]models.MessageAttachment, error) {
	query := `
		SELECT id, dm_message_id, uploader_id, filename, file_url, file_size, content_type, thumbnail_url, width, height, created_at
		FROM message_attachments
		WHERE dm_message_id = $1`

	rows, err := s.db.Query(ctx, query, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attachments []models.MessageAttachment
	for rows.Next() {
		var a models.MessageAttachment
		var dmMessageID *uuid.UUID
		err := rows.Scan(&a.ID, &dmMessageID, &a.UploaderID, &a.Filename, &a.FileURL,
			&a.FileSize, &a.ContentType, &a.ThumbnailURL, &a.Width, &a.Height, &a.CreatedAt)
		if err != nil {
			return nil, err
		}
		if dmMessageID != nil {
			a.MessageID = dmMessageID
		}
		attachments = append(attachments, a)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating over DM message attachments")
		return nil, err
	}

	return attachments, nil
}

func (s *Service) batchGetDmAttachments(ctx context.Context, messageIDs []uuid.UUID) map[uuid.UUID][]models.MessageAttachment {
	result := make(map[uuid.UUID][]models.MessageAttachment)

	query := `
		SELECT id, dm_message_id, uploader_id, filename, file_url, file_size, content_type, thumbnail_url, width, height, created_at
		FROM message_attachments
		WHERE dm_message_id = ANY($1)`

	rows, err := s.db.Query(ctx, query, messageIDs)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var a models.MessageAttachment
		var dmMessageID *uuid.UUID
		err := rows.Scan(&a.ID, &dmMessageID, &a.UploaderID, &a.Filename, &a.FileURL,
			&a.FileSize, &a.ContentType, &a.ThumbnailURL, &a.Width, &a.Height, &a.CreatedAt)
		if err != nil {
			continue
		}
		if dmMessageID != nil {
			a.MessageID = dmMessageID
			result[*dmMessageID] = append(result[*dmMessageID], a)
		}
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating over batch DM attachments")
	}

	return result
}
