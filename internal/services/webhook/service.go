package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
	"github.com/zentra/server/internal/services/media"
	"github.com/zentra/server/internal/services/message"
	"github.com/zentra/server/internal/services/messaging"
	"github.com/zentra/server/pkg/auth"
	"github.com/zentra/server/pkg/encryption"
)

const (
	MaxPayloadBytes = 1024 * 1024
)

var (
	ErrWebhookNotFound          = errors.New("webhook not found")
	ErrWebhookInactive          = errors.New("webhook inactive")
	ErrInvalidWebhookToken      = errors.New("invalid webhook token")
	ErrWebhookInsufficientPerms = errors.New("insufficient permissions")
	ErrChannelNotFound          = errors.New("channel not found")
	ErrWebhookAvatarTooLarge    = errors.New("webhook avatar too large")
	ErrInvalidWebhookAvatar     = errors.New("invalid webhook avatar")
)

type ChannelServiceInterface interface {
	CanManageWebhooks(ctx context.Context, channelID, userID uuid.UUID) bool
}

type AvatarUploader interface {
	UploadAvatar(ctx context.Context, ownerID uuid.UUID, ownerType string, file multipart.File, header *multipart.FileHeader) (string, error)
}

type Service struct {
	db             *pgxpool.Pool
	redis          *redis.Client
	cipher         messaging.ContentCipher
	channelService ChannelServiceInterface
	avatarUploader AvatarUploader
	masterKey      []byte
}

type CreateWebhookRequest struct {
	Name         string  `json:"name"`
	AvatarURL    *string `json:"avatarUrl"`
	ProviderHint *string `json:"providerHint"`
}

type UpdateWebhookRequest struct {
	Name         *string `json:"name"`
	AvatarURL    *string `json:"avatarUrl"`
	ProviderHint *string `json:"providerHint"`
	ChannelID    *string `json:"channelId"`
	IsActive     *bool   `json:"isActive"`
}

func NewService(db *pgxpool.Pool, redisClient *redis.Client, encryptionKey []byte, channelService ChannelServiceInterface, avatarUploader AvatarUploader) *Service {
	return &Service{
		db:             db,
		redis:          redisClient,
		cipher:         messaging.NewChannelCipher(),
		channelService: channelService,
		avatarUploader: avatarUploader,
		masterKey:      encryptionKey,
	}
}

func (s *Service) communityDEK(ctx context.Context, channelID uuid.UUID) ([]byte, error) {
	var wrapped []byte
	err := s.db.QueryRow(ctx,
		`SELECT c.encrypted_dek
		 FROM communities c
		 JOIN channels ch ON ch.community_id = c.id
		 WHERE ch.id = $1`, channelID,
	).Scan(&wrapped)
	if err != nil {
		return nil, fmt.Errorf("load community DEK: %w", err)
	}
	return encryption.UnwrapKey(wrapped, s.masterKey)
}

func (s *Service) CreateWebhook(ctx context.Context, channelID, userID uuid.UUID, req *CreateWebhookRequest) (*models.Webhook, string, error) {
	if !s.channelService.CanManageWebhooks(ctx, channelID, userID) {
		return nil, "", ErrWebhookInsufficientPerms
	}

	communityID, err := s.getChannelCommunityID(ctx, channelID)
	if err != nil {
		return nil, "", err
	}

	name := sanitizeWebhookName("")
	avatarURL := sanitizeOptionalString(nil, 2048)
	providerHint := sanitizeProviderHint(nil)
	if req != nil {
		name = sanitizeWebhookName(req.Name)
		avatarURL = sanitizeOptionalString(req.AvatarURL, 2048)
		providerHint = sanitizeProviderHint(req.ProviderHint)
	}

	webhookID := uuid.New()
	token, err := generateWebhookToken()
	if err != nil {
		return nil, "", fmt.Errorf("generate webhook token: %w", err)
	}

	now := time.Now()
	webhook := &models.Webhook{
		ID:           webhookID,
		ChannelID:    channelID,
		CommunityID:  communityID,
		CreatedBy:    userID,
		Name:         name,
		AvatarURL:    avatarURL,
		ProviderHint: providerHint,
		TokenHash:    hashWebhookToken(token),
		TokenPreview: tokenPreview(token),
		IsActive:     true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)

	botUserID, err := s.createWebhookBotUser(ctx, tx, webhookID, name, avatarURL, now)
	if err != nil {
		return nil, "", err
	}
	webhook.BotUserID = botUserID

	_, err = tx.Exec(ctx,
		`INSERT INTO webhooks (id, channel_id, community_id, created_by, bot_user_id, name, avatar_url, provider_hint, token_hash, token_preview, is_active, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, TRUE, $11, $11)`,
		webhook.ID, webhook.ChannelID, webhook.CommunityID, webhook.CreatedBy, webhook.BotUserID,
		webhook.Name, webhook.AvatarURL, webhook.ProviderHint, webhook.TokenHash, webhook.TokenPreview, webhook.CreatedAt,
	)
	if err != nil {
		return nil, "", fmt.Errorf("insert webhook: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}

	return webhook, token, nil
}

func (s *Service) UploadWebhookAvatar(ctx context.Context, channelID, userID uuid.UUID, file multipart.File, header *multipart.FileHeader) (string, error) {
	if !s.channelService.CanManageWebhooks(ctx, channelID, userID) {
		return "", ErrWebhookInsufficientPerms
	}

	if s.avatarUploader == nil {
		return "", fmt.Errorf("avatar uploader unavailable")
	}

	url, err := s.avatarUploader.UploadAvatar(ctx, uuid.New(), "webhooks", file, header)
	if err != nil {
		switch {
		case errors.Is(err, media.ErrFileTooLarge):
			return "", ErrWebhookAvatarTooLarge
		case errors.Is(err, media.ErrInvalidFileType):
			return "", ErrInvalidWebhookAvatar
		default:
			return "", fmt.Errorf("upload webhook avatar: %w", err)
		}
	}

	return url, nil
}

func (s *Service) ListChannelWebhooks(ctx context.Context, channelID, userID uuid.UUID) ([]*models.Webhook, error) {
	if !s.channelService.CanManageWebhooks(ctx, channelID, userID) {
		return nil, ErrWebhookInsufficientPerms
	}

	rows, err := s.db.Query(ctx,
		`SELECT id, channel_id, community_id, created_by, bot_user_id, name, avatar_url, provider_hint,
		        token_hash, token_preview, is_active, last_used_at, created_at, updated_at
		 FROM webhooks
		 WHERE channel_id = $1
		 ORDER BY created_at ASC`,
		channelID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	webhooks := make([]*models.Webhook, 0)
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		webhooks = append(webhooks, w)
	}

	return webhooks, rows.Err()
}

func (s *Service) UpdateWebhook(ctx context.Context, webhookID, userID uuid.UUID, req *UpdateWebhookRequest) (*models.Webhook, error) {
	if req == nil {
		req = &UpdateWebhookRequest{}
	}

	webhook, err := s.getWebhook(ctx, webhookID)
	if err != nil {
		return nil, err
	}
	if !s.channelService.CanManageWebhooks(ctx, webhook.ChannelID, userID) {
		return nil, ErrWebhookInsufficientPerms
	}

	channelID := webhook.ChannelID
	name := webhook.Name
	avatarURL := webhook.AvatarURL
	providerHint := webhook.ProviderHint
	isActive := webhook.IsActive
	communityID := webhook.CommunityID

	if req.ChannelID != nil {
		nextChannelID, parseErr := uuid.Parse(strings.TrimSpace(*req.ChannelID))
		if parseErr != nil {
			return nil, ErrChannelNotFound
		}

		nextCommunityID, communityErr := s.getChannelCommunityID(ctx, nextChannelID)
		if communityErr != nil {
			if errors.Is(communityErr, ErrChannelNotFound) {
				return nil, ErrChannelNotFound
			}
			return nil, communityErr
		}

		if nextCommunityID != webhook.CommunityID {
			return nil, ErrChannelNotFound
		}

		if nextChannelID != webhook.ChannelID && !s.channelService.CanManageWebhooks(ctx, nextChannelID, userID) {
			return nil, ErrWebhookInsufficientPerms
		}

		channelID = nextChannelID
		communityID = nextCommunityID
	}

	if req.Name != nil {
		name = sanitizeWebhookName(*req.Name)
	}
	if req.AvatarURL != nil {
		avatarURL = sanitizeOptionalString(req.AvatarURL, 2048)
	}
	if req.ProviderHint != nil {
		providerHint = sanitizeProviderHint(req.ProviderHint)
	}
	if req.IsActive != nil {
		isActive = *req.IsActive
	}

	now := time.Now()
	_, err = s.db.Exec(ctx,
		`UPDATE webhooks
		 SET channel_id = $2, community_id = $3, name = $4, avatar_url = $5, provider_hint = $6, is_active = $7, updated_at = $8
		 WHERE id = $1`,
		webhookID, channelID, communityID, name, avatarURL, providerHint, isActive, now,
	)
	if err != nil {
		return nil, err
	}

	_, err = s.db.Exec(ctx,
		`UPDATE users SET display_name = $2, avatar_url = $3, updated_at = $4 WHERE id = $1`,
		webhook.BotUserID, name, avatarURL, now,
	)
	if err != nil {
		return nil, err
	}

	return s.getWebhook(ctx, webhookID)
}

func (s *Service) RotateWebhookToken(ctx context.Context, webhookID, userID uuid.UUID) (*models.Webhook, string, error) {
	webhook, err := s.getWebhook(ctx, webhookID)
	if err != nil {
		return nil, "", err
	}
	if !s.channelService.CanManageWebhooks(ctx, webhook.ChannelID, userID) {
		return nil, "", ErrWebhookInsufficientPerms
	}

	token, err := generateWebhookToken()
	if err != nil {
		return nil, "", fmt.Errorf("generate webhook token: %w", err)
	}

	now := time.Now()
	_, err = s.db.Exec(ctx,
		`UPDATE webhooks
		 SET token_hash = $2, token_preview = $3, updated_at = $4
		 WHERE id = $1`,
		webhookID, hashWebhookToken(token), tokenPreview(token), now,
	)
	if err != nil {
		return nil, "", err
	}

	updated, err := s.getWebhook(ctx, webhookID)
	if err != nil {
		return nil, "", err
	}

	return updated, token, nil
}

func (s *Service) DeleteWebhook(ctx context.Context, webhookID, userID uuid.UUID) error {
	webhook, err := s.getWebhook(ctx, webhookID)
	if err != nil {
		return err
	}
	if !s.channelService.CanManageWebhooks(ctx, webhook.ChannelID, userID) {
		return ErrWebhookInsufficientPerms
	}

	tag, err := s.db.Exec(ctx, `DELETE FROM webhooks WHERE id = $1`, webhookID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWebhookNotFound
	}
	return nil
}

func (s *Service) ExecuteWebhook(ctx context.Context, webhookID uuid.UUID, token string, headers http.Header, contentType string, rawBody []byte) (*message.MessageResponse, error) {
	webhook, err := s.getWebhook(ctx, webhookID)
	if err != nil {
		return nil, err
	}
	if !webhook.IsActive {
		return nil, ErrWebhookInactive
	}

	if !secureCompareTokens(webhook.TokenHash, hashWebhookToken(token)) {
		return nil, ErrInvalidWebhookToken
	}

	webhookKey, err := s.communityDEK(ctx, webhook.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("derive webhook channel key: %w", err)
	}

	content, previews := buildWebhookMessage(webhook, headers, contentType, rawBody)
	encryptedContent, _, err := s.cipher.Encrypt(content, webhookKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt webhook message: %w", err)
	}

	linkPreviewJSON := messaging.EncodeLinkPreviews(previews)
	messageID := uuid.New()
	now := time.Now()

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx,
		`INSERT INTO messages (id, channel_id, author_id, encrypted_content, link_previews, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
		messageID, webhook.ChannelID, webhook.BotUserID, encryptedContent, string(linkPreviewJSON), now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert webhook message: %w", err)
	}

	_, err = tx.Exec(ctx,
		`UPDATE channels SET last_message_at = $1 WHERE id = $2`,
		now, webhook.ChannelID,
	)
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx,
		`UPDATE webhooks SET last_used_at = $2, updated_at = $2 WHERE id = $1`,
		webhookID, now,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	resp, err := s.getMessageResponse(ctx, messageID)
	if err != nil {
		return nil, err
	}

	s.broadcast(ctx, webhook.ChannelID.String(), "MESSAGE_CREATE", resp)

	return resp, nil
}

func (s *Service) getChannelCommunityID(ctx context.Context, channelID uuid.UUID) (uuid.UUID, error) {
	var communityID uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT community_id FROM channels WHERE id = $1`, channelID).Scan(&communityID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrChannelNotFound
		}
		return uuid.Nil, err
	}
	return communityID, nil
}

func (s *Service) createWebhookBotUser(ctx context.Context, tx pgx.Tx, webhookID uuid.UUID, displayName string, avatarURL *string, now time.Time) (uuid.UUID, error) {
	botUserID := uuid.New()
	compactID := strings.ReplaceAll(webhookID.String(), "-", "")
	username := "wh" + compactID[:30]
	email := fmt.Sprintf("%s@webhook.zentra.local", username)

	passwordHash, err := auth.HashPassword(uuid.NewString() + ":webhook")
	if err != nil {
		return uuid.Nil, err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO users (id, username, email, password_hash, display_name, avatar_url, status, email_verified, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, $8, $8)`,
		botUserID, username, email, passwordHash, displayName, avatarURL, models.UserStatusOffline, now,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create webhook bot user: %w", err)
	}

	return botUserID, nil
}

func (s *Service) getWebhook(ctx context.Context, webhookID uuid.UUID) (*models.Webhook, error) {
	row := s.db.QueryRow(ctx,
		`SELECT id, channel_id, community_id, created_by, bot_user_id, name, avatar_url, provider_hint,
		        token_hash, token_preview, is_active, last_used_at, created_at, updated_at
		 FROM webhooks
		 WHERE id = $1`,
		webhookID,
	)

	webhook, err := scanWebhook(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWebhookNotFound
		}
		return nil, err
	}
	return webhook, nil
}

func scanWebhook(scanner interface{ Scan(dest ...any) error }) (*models.Webhook, error) {
	w := &models.Webhook{}
	err := scanner.Scan(
		&w.ID,
		&w.ChannelID,
		&w.CommunityID,
		&w.CreatedBy,
		&w.BotUserID,
		&w.Name,
		&w.AvatarURL,
		&w.ProviderHint,
		&w.TokenHash,
		&w.TokenPreview,
		&w.IsActive,
		&w.LastUsedAt,
		&w.CreatedAt,
		&w.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (s *Service) getMessageResponse(ctx context.Context, messageID uuid.UUID) (*message.MessageResponse, error) {
	query := `
		SELECT m.id, m.channel_id, m.author_id, m.encrypted_content, m.reply_to_id,
		       m.link_previews, m.is_pinned, m.is_edited, m.reactions, m.created_at, m.updated_at,
		       u.id, u.username, u.display_name, u.avatar_url, u.banner_url, u.bio, u.status, u.custom_status, u.created_at
		FROM messages m
		JOIN users u ON u.id = m.author_id
		WHERE m.id = $1 AND m.deleted_at IS NULL`

	var msg models.Message
	var encContent []byte
	var linkPreviewRaw []byte
	var author models.PublicUser

	err := s.db.QueryRow(ctx, query, messageID).Scan(
		&msg.ID, &msg.ChannelID, &msg.AuthorID, &encContent,
		&msg.ReplyToID, &linkPreviewRaw, &msg.IsPinned, &msg.IsEdited, &msg.Reactions, &msg.CreatedAt, &msg.UpdatedAt,
		&author.ID, &author.Username, &author.DisplayName, &author.AvatarURL, &author.BannerURL, &author.Bio, &author.Status, &author.CustomStatus, &author.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, message.ErrMessageNotFound
		}
		return nil, err
	}

	msgKey, err := s.communityDEK(ctx, msg.ChannelID)
	if err != nil {
		return nil, err
	}

	content, err := s.cipher.Decrypt(encContent, nil, msgKey)
	if err != nil {
		fallback := "[Decryption Error]"
		msg.Content = &fallback
	} else {
		msg.Content = &content
	}
	msg.LinkPreviews = messaging.DecodeLinkPreviews(linkPreviewRaw)

	resp := &message.MessageResponse{
		Message:   &msg,
		Author:    &author,
		Reactions: make([]message.ReactionSummary, 0),
	}

	for emoji, users := range msg.Reactions {
		if len(users) == 0 {
			continue
		}
		resp.Reactions = append(resp.Reactions, message.ReactionSummary{
			Emoji:   emoji,
			Count:   len(users),
			Users:   users,
			Reacted: false,
		})
	}

	return resp, nil
}

func (s *Service) broadcast(ctx context.Context, channelID string, eventType string, data any) {
	event := struct {
		Type string `json:"type"`
		Data any    `json:"data"`
	}{
		Type: eventType,
		Data: data,
	}

	broadcast := struct {
		ChannelID string `json:"channelId"`
		Event     any    `json:"event"`
	}{
		ChannelID: channelID,
		Event:     event,
	}

	jsonData, err := json.Marshal(broadcast)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal webhook broadcast event")
		return
	}

	if err := s.redis.Publish(ctx, "websocket:broadcast", jsonData).Err(); err != nil {
		log.Error().Err(err).Msg("Failed to publish webhook broadcast")
	}
}

func buildWebhookMessage(webhook *models.Webhook, headers http.Header, contentType string, rawBody []byte) (string, []models.LinkPreview) {
	payload, rawText := parseWebhookPayload(contentType, rawBody)
	provider, event := detectProvider(headers, webhook.ProviderHint, payload)

	if provider == "github" {
		content, previews := formatGitHubPayload(event, payload)
		if strings.TrimSpace(content) != "" {
			return finalizeWebhookMessage(content, previews)
		}
	}

	content := ""
	if payload != nil {
		content = firstString(payload, "content", "text", "message")
	}
	if content == "" && strings.TrimSpace(rawText) != "" && payload == nil {
		content = rawText
	}

	previews := make([]models.LinkPreview, 0)
	if payload != nil {
		previews = append(previews, extractEmbeds(payload)...)
		if len(previews) == 0 {
			previews = append(previews, buildDefaultPreview(provider, event, payload)...)
		}
	}

	if strings.TrimSpace(content) == "" {
		switch {
		case provider != "" && event != "":
			content = fmt.Sprintf("%s webhook event: %s", providerDisplayName(provider), event)
		case provider != "":
			content = fmt.Sprintf("%s webhook event received", providerDisplayName(provider))
		default:
			content = "Webhook event received"
		}
	}

	return finalizeWebhookMessage(content, previews)
}

func finalizeWebhookMessage(content string, previews []models.LinkPreview) (string, []models.LinkPreview) {
	cleaned := truncateText(strings.TrimSpace(content), 4000)
	if cleaned == "" {
		cleaned = "Webhook event received"
	}

	finalPreviews := make([]models.LinkPreview, 0, len(previews))
	for _, preview := range previews {
		url := strings.TrimSpace(preview.URL)
		if url == "" {
			continue
		}
		finalPreviews = append(finalPreviews, models.LinkPreview{
			URL:         url,
			Title:       truncateText(preview.Title, 180),
			Description: truncateText(preview.Description, 400),
			SiteName:    truncateText(preview.SiteName, 80),
			ImageURL:    strings.TrimSpace(preview.ImageURL),
			FaviconURL:  strings.TrimSpace(preview.FaviconURL),
		})
		if len(finalPreviews) >= 5 {
			break
		}
	}

	return cleaned, finalPreviews
}

func formatGitHubPayload(event string, payload map[string]any) (string, []models.LinkPreview) {
	if payload == nil {
		return "", nil
	}

	repo := asMap(payload["repository"])
	repoName := firstString(repo, "full_name")
	repoURL := firstString(repo, "html_url")
	repoDescription := firstString(repo, "description")
	sender := nestedString(payload, "sender", "login")
	if sender == "" {
		sender = "GitHub"
	}
	senderAvatar := nestedString(payload, "sender", "avatar_url")
	action := strings.TrimSpace(strings.ToLower(firstString(payload, "action")))

	event = strings.TrimSpace(strings.ToLower(event))
	switch event {
	case "push":
		ref := firstString(payload, "ref")
		branch := strings.TrimPrefix(ref, "refs/heads/")
		commits := asSlice(payload["commits"])
		commitCount := len(commits)
		if commitCount == 0 {
			commitCount = intFromAny(payload["size"])
		}

		summary := fmt.Sprintf("%s pushed", sender)
		if commitCount > 0 {
			summary += fmt.Sprintf(" %d commit", commitCount)
			if commitCount != 1 {
				summary += "s"
			}
		}
		if branch != "" {
			summary += fmt.Sprintf(" to %s", branch)
		}
		if repoName != "" {
			summary += fmt.Sprintf(" in %s", repoName)
		}

		description := "Repository updated"
		if len(commits) > 0 {
			if firstCommit := asMap(commits[0]); firstCommit != nil {
				description = firstString(firstCommit, "message")
			}
		}
		if description == "Repository updated" {
			headCommit := asMap(payload["head_commit"])
			if headCommit != nil {
				description = firstString(headCommit, "message")
			}
		}

		previewURL := firstNonEmpty(firstString(payload, "compare"), repoURL, "https://github.com")
		title := firstNonEmpty(repoName, "GitHub push")
		if branch != "" {
			title = fmt.Sprintf("%s (%s)", title, branch)
		}

		return summary, []models.LinkPreview{{
			URL:         previewURL,
			Title:       title,
			Description: description,
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "pull_request":
		pr := asMap(payload["pull_request"])
		number := intFromAny(payload["number"])
		prTitle := firstString(pr, "title")
		prURL := firstNonEmpty(firstString(pr, "html_url"), repoURL, "https://github.com")
		prBody := firstString(pr, "body")
		merged := false
		if mergedFlag, ok := pr["merged"].(bool); ok {
			merged = mergedFlag
		}
		verb := githubPullRequestVerb(action, merged)

		summary := fmt.Sprintf("%s %s pull request", sender, verb)
		if number > 0 {
			summary += fmt.Sprintf(" #%d", number)
		}
		if repoName != "" {
			summary += fmt.Sprintf(" in %s", repoName)
		}
		if prTitle != "" {
			summary += fmt.Sprintf(": %s", prTitle)
		}

		title := prTitle
		if title == "" {
			title = "Pull request updated"
		}
		if number > 0 {
			title = fmt.Sprintf("PR #%d: %s", number, title)
		}

		return summary, []models.LinkPreview{{
			URL:         prURL,
			Title:       title,
			Description: firstNonEmpty(prBody, fmt.Sprintf("Pull request %s", verb)),
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "issues":
		issue := asMap(payload["issue"])
		number := intFromAny(payload["number"])
		issueTitle := firstString(issue, "title")
		issueURL := firstNonEmpty(firstString(issue, "html_url"), repoURL, "https://github.com")
		issueBody := firstString(issue, "body")
		verb := githubIssueVerb(action)

		summary := fmt.Sprintf("%s %s issue", sender, verb)
		if number > 0 {
			summary += fmt.Sprintf(" #%d", number)
		}
		if repoName != "" {
			summary += fmt.Sprintf(" in %s", repoName)
		}
		if issueTitle != "" {
			summary += fmt.Sprintf(": %s", issueTitle)
		}

		title := issueTitle
		if title == "" {
			title = "Issue updated"
		}
		if number > 0 {
			title = fmt.Sprintf("Issue #%d: %s", number, title)
		}

		return summary, []models.LinkPreview{{
			URL:         issueURL,
			Title:       title,
			Description: firstNonEmpty(issueBody, fmt.Sprintf("Issue %s", verb)),
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "issue_comment":
		issue := asMap(payload["issue"])
		comment := asMap(payload["comment"])
		number := intFromAny(issue["number"])
		issueTitle := firstString(issue, "title")
		issueURL := firstNonEmpty(firstString(issue, "html_url"), repoURL, "https://github.com")
		commentBody := firstString(comment, "body")

		verb := "commented on"
		switch action {
		case "edited":
			verb = "edited a comment on"
		case "deleted":
			verb = "deleted a comment on"
		}

		summary := fmt.Sprintf("%s %s issue", sender, verb)
		if number > 0 {
			summary += fmt.Sprintf(" #%d", number)
		}
		if repoName != "" {
			summary += fmt.Sprintf(" in %s", repoName)
		}

		title := issueTitle
		if title == "" {
			title = "Issue comment"
		}
		if number > 0 {
			title = fmt.Sprintf("Issue #%d: %s", number, title)
		}

		return summary, []models.LinkPreview{{
			URL:         issueURL,
			Title:       title,
			Description: firstNonEmpty(commentBody, "Issue comment activity"),
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "pull_request_review":
		pr := asMap(payload["pull_request"])
		review := asMap(payload["review"])
		number := intFromAny(payload["number"])
		prTitle := firstString(pr, "title")
		prURL := firstNonEmpty(firstString(pr, "html_url"), repoURL, "https://github.com")
		reviewState := strings.TrimSpace(strings.ToLower(firstString(review, "state")))
		reviewBody := firstString(review, "body")

		reviewVerb := "reviewed"
		switch reviewState {
		case "approved":
			reviewVerb = "approved"
		case "changes_requested":
			reviewVerb = "requested changes on"
		}

		summary := fmt.Sprintf("%s %s pull request", sender, reviewVerb)
		if number > 0 {
			summary += fmt.Sprintf(" #%d", number)
		}
		if repoName != "" {
			summary += fmt.Sprintf(" in %s", repoName)
		}
		if prTitle != "" {
			summary += fmt.Sprintf(": %s", prTitle)
		}

		title := prTitle
		if title == "" {
			title = "Pull request review"
		}
		if number > 0 {
			title = fmt.Sprintf("PR #%d: %s", number, title)
		}

		description := firstNonEmpty(reviewBody, fmt.Sprintf("Review state: %s", firstNonEmpty(reviewState, "commented")))

		return summary, []models.LinkPreview{{
			URL:         prURL,
			Title:       title,
			Description: description,
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "pull_request_review_comment":
		pr := asMap(payload["pull_request"])
		comment := asMap(payload["comment"])
		number := intFromAny(payload["number"])
		prTitle := firstString(pr, "title")
		prURL := firstNonEmpty(firstString(pr, "html_url"), repoURL, "https://github.com")
		commentBody := firstString(comment, "body")

		verb := "commented on"
		switch action {
		case "edited":
			verb = "edited a comment on"
		case "deleted":
			verb = "deleted a comment on"
		}

		summary := fmt.Sprintf("%s %s pull request", sender, verb)
		if number > 0 {
			summary += fmt.Sprintf(" #%d", number)
		}
		if repoName != "" {
			summary += fmt.Sprintf(" in %s", repoName)
		}

		title := prTitle
		if title == "" {
			title = "Pull request comment"
		}
		if number > 0 {
			title = fmt.Sprintf("PR #%d: %s", number, title)
		}

		return summary, []models.LinkPreview{{
			URL:         prURL,
			Title:       title,
			Description: firstNonEmpty(commentBody, "Pull request comment activity"),
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "star":
		starVerb := "starred"
		if action == "deleted" {
			starVerb = "unstarred"
		}

		summary := fmt.Sprintf("%s %s %s", sender, starVerb, firstNonEmpty(repoName, "a repository"))
		stars := intFromAny(repo["stargazers_count"])
		description := firstNonEmpty(repoDescription, "Repository star activity")
		if stars > 0 {
			description = fmt.Sprintf("%d stars", stars)
		}

		return summary, []models.LinkPreview{{
			URL:         firstNonEmpty(repoURL, "https://github.com"),
			Title:       firstNonEmpty(repoName, "Repository stars"),
			Description: description,
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "watch":
		watchVerb := "updated watch status for"
		switch action {
		case "started":
			watchVerb = "started watching"
		case "deleted", "stopped":
			watchVerb = "stopped watching"
		}

		summary := fmt.Sprintf("%s %s %s", sender, watchVerb, firstNonEmpty(repoName, "a repository"))
		watchers := intFromAny(repo["subscribers_count"])
		description := firstNonEmpty(repoDescription, "Repository watch activity")
		if watchers > 0 {
			description = fmt.Sprintf("%d watchers", watchers)
		}

		return summary, []models.LinkPreview{{
			URL:         firstNonEmpty(repoURL, "https://github.com"),
			Title:       firstNonEmpty(repoName, "Repository watchers"),
			Description: description,
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "fork":
		forkee := asMap(payload["forkee"])
		forkName := firstString(forkee, "full_name")
		forkURL := firstString(forkee, "html_url")
		forkDescription := firstString(forkee, "description")

		summary := fmt.Sprintf("%s forked %s", sender, firstNonEmpty(repoName, "a repository"))
		if forkName != "" {
			summary += fmt.Sprintf(" to %s", forkName)
		}

		title := firstNonEmpty(forkName, repoName, "Fork created")
		description := firstNonEmpty(forkDescription, "Repository fork created")

		return summary, []models.LinkPreview{{
			URL:         firstNonEmpty(forkURL, repoURL, "https://github.com"),
			Title:       title,
			Description: description,
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "create":
		refType := firstNonEmpty(firstString(payload, "ref_type"), "resource")
		refName := firstString(payload, "ref")

		target := strings.TrimSpace(refType)
		if refName != "" {
			target += fmt.Sprintf(" %s", refName)
		}
		target = strings.TrimSpace(target)
		if target == "" {
			target = "resource"
		}

		summary := fmt.Sprintf("%s created %s", sender, target)
		if repoName != "" {
			summary += fmt.Sprintf(" in %s", repoName)
		}

		title := firstNonEmpty(repoName, "GitHub repository")
		description := fmt.Sprintf("Created %s", target)

		return summary, []models.LinkPreview{{
			URL:         firstNonEmpty(repoURL, "https://github.com"),
			Title:       title,
			Description: description,
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "delete":
		refType := firstNonEmpty(firstString(payload, "ref_type"), "resource")
		refName := firstString(payload, "ref")

		target := strings.TrimSpace(refType)
		if refName != "" {
			target += fmt.Sprintf(" %s", refName)
		}
		target = strings.TrimSpace(target)
		if target == "" {
			target = "resource"
		}

		summary := fmt.Sprintf("%s deleted %s", sender, target)
		if repoName != "" {
			summary += fmt.Sprintf(" in %s", repoName)
		}

		title := firstNonEmpty(repoName, "GitHub repository")
		description := fmt.Sprintf("Deleted %s", target)

		return summary, []models.LinkPreview{{
			URL:         firstNonEmpty(repoURL, "https://github.com"),
			Title:       title,
			Description: description,
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "release":
		release := asMap(payload["release"])
		tag := firstNonEmpty(firstString(release, "tag_name"), firstString(release, "name"))
		releaseURL := firstNonEmpty(firstString(release, "html_url"), repoURL, "https://github.com")
		releaseBody := firstString(release, "body")

		summary := fmt.Sprintf("%s %s release", sender, safeAction(action))
		if tag != "" {
			summary += fmt.Sprintf(" %s", tag)
		}
		if repoName != "" {
			summary += fmt.Sprintf(" in %s", repoName)
		}

		title := firstNonEmpty(firstString(release, "name"), tag, "Release updated")

		return summary, []models.LinkPreview{{
			URL:         releaseURL,
			Title:       title,
			Description: firstNonEmpty(releaseBody, "Release activity"),
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	case "ping":
		zen := firstString(payload, "zen")
		summary := "GitHub webhook endpoint verified"
		if repoName != "" {
			summary = fmt.Sprintf("GitHub webhook verified for %s", repoName)
		}

		description := "Webhook endpoint successfully verified"
		if zen != "" {
			description = fmt.Sprintf("Ping response: %s", zen)
		}

		return summary, []models.LinkPreview{{
			URL:         firstNonEmpty(repoURL, "https://github.com"),
			Title:       firstNonEmpty(repoName, "GitHub webhook"),
			Description: description,
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	default:
		eventLabel := strings.ReplaceAll(strings.TrimSpace(event), "_", " ")
		if eventLabel == "" {
			eventLabel = "event"
		}

		summary := fmt.Sprintf("%s triggered GitHub %s", sender, eventLabel)
		if repoName != "" {
			summary += fmt.Sprintf(" in %s", repoName)
		}

		detail := "GitHub webhook activity"
		if action != "" {
			detail = fmt.Sprintf("Action: %s", strings.ReplaceAll(action, "_", " "))
		}

		description := firstNonEmpty(
			detail,
			firstString(payload, "zen"),
			summarizePayload(payload),
		)

		title := firstNonEmpty(repoName, fmt.Sprintf("GitHub %s", eventLabel))
		return summary, []models.LinkPreview{{
			URL:         firstNonEmpty(repoURL, "https://github.com"),
			Title:       title,
			Description: description,
			SiteName:    "GitHub",
			ImageURL:    senderAvatar,
		}}
	}
}

func parseWebhookPayload(contentType string, rawBody []byte) (map[string]any, string) {
	trimmed := bytes.TrimSpace(rawBody)
	if len(trimmed) == 0 {
		return nil, ""
	}

	rawText := string(trimmed)
	lowerContentType := strings.ToLower(strings.TrimSpace(contentType))

	if strings.Contains(lowerContentType, "application/x-www-form-urlencoded") {
		formValues, err := url.ParseQuery(rawText)
		if err == nil {
			if wrapped := strings.TrimSpace(formValues.Get("payload")); wrapped != "" {
				var wrappedPayload map[string]any
				if err := json.Unmarshal([]byte(wrapped), &wrappedPayload); err == nil {
					return wrappedPayload, wrapped
				}
			}

			payload := make(map[string]any, len(formValues))
			for key, values := range formValues {
				if len(values) == 1 {
					payload[key] = values[0]
					continue
				}
				items := make([]any, 0, len(values))
				for _, value := range values {
					items = append(items, value)
				}
				payload[key] = items
			}
			return payload, rawText
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(trimmed, &payload); err == nil {
		return payload, rawText
	}

	var payloadArray []any
	if err := json.Unmarshal(trimmed, &payloadArray); err == nil {
		return map[string]any{"items": payloadArray}, rawText
	}

	return nil, rawText
}

func detectProvider(headers http.Header, providerHint *string, payload map[string]any) (string, string) {
	provider := strings.TrimSpace(strings.ToLower(headers.Get("X-Webhook-Provider")))
	event := strings.TrimSpace(headers.Get("X-Webhook-Event"))

	if githubEvent := strings.TrimSpace(headers.Get("X-GitHub-Event")); githubEvent != "" {
		provider = "github"
		event = githubEvent
	}
	if gitlabEvent := strings.TrimSpace(headers.Get("X-Gitlab-Event")); gitlabEvent != "" {
		if provider == "" {
			provider = "gitlab"
		}
		if event == "" {
			event = gitlabEvent
		}
	}

	if provider == "" && providerHint != nil {
		provider = strings.TrimSpace(strings.ToLower(*providerHint))
	}

	if provider == "" && payload != nil {
		if payload["repository"] != nil && payload["sender"] != nil {
			provider = "github"
		}
		if event == "" {
			event = firstString(payload, "event", "type")
		}
	}

	return provider, event
}

func extractEmbeds(payload map[string]any) []models.LinkPreview {
	rawEmbeds := asSlice(payload["embeds"])
	if len(rawEmbeds) == 0 {
		embed := asMap(payload["embed"])
		if embed != nil {
			rawEmbeds = []any{embed}
		}
	}

	previews := make([]models.LinkPreview, 0, len(rawEmbeds))
	for _, rawEmbed := range rawEmbeds {
		embed := asMap(rawEmbed)
		if embed == nil {
			continue
		}

		image := firstNonEmpty(
			firstString(embed, "imageUrl", "image_url"),
			nestedString(embed, "image", "url"),
			nestedString(embed, "thumbnail", "url"),
		)

		preview := models.LinkPreview{
			URL:         firstNonEmpty(firstString(embed, "url", "link"), nestedString(embed, "author", "url"), "https://zentra.chat"),
			Title:       firstNonEmpty(firstString(embed, "title", "name"), nestedString(embed, "author", "name")),
			Description: firstNonEmpty(firstString(embed, "description", "summary", "text"), firstString(embed, "body")),
			SiteName:    firstNonEmpty(firstString(embed, "siteName", "site_name", "provider", "source"), providerDisplayName(firstString(embed, "provider"))),
			ImageURL:    image,
			FaviconURL:  firstString(embed, "faviconUrl", "favicon_url"),
		}

		previews = append(previews, preview)
		if len(previews) >= 5 {
			break
		}
	}

	return previews
}

func buildDefaultPreview(provider string, event string, payload map[string]any) []models.LinkPreview {
	if payload == nil {
		return nil
	}

	previewURL := firstNonEmpty(
		firstString(payload, "url", "html_url", "target_url", "link"),
		nestedString(payload, "repository", "html_url"),
		nestedString(payload, "project", "web_url"),
		nestedString(payload, "pull_request", "html_url"),
		nestedString(payload, "issue", "html_url"),
		nestedString(payload, "release", "html_url"),
	)
	if previewURL == "" {
		return nil
	}

	title := firstNonEmpty(
		firstString(payload, "title", "subject"),
		nestedString(payload, "repository", "full_name"),
	)
	if title == "" {
		switch {
		case provider != "" && event != "":
			title = fmt.Sprintf("%s %s", providerDisplayName(provider), event)
		case provider != "":
			title = fmt.Sprintf("%s webhook", providerDisplayName(provider))
		default:
			title = "Webhook event"
		}
	}

	description := firstNonEmpty(
		firstString(payload, "description", "summary"),
		nestedString(payload, "head_commit", "message"),
		nestedString(payload, "pull_request", "title"),
		nestedString(payload, "issue", "title"),
		nestedString(payload, "release", "name"),
		summarizePayload(payload),
	)

	siteName := ""
	if provider != "" {
		siteName = providerDisplayName(provider)
	}

	return []models.LinkPreview{{
		URL:         previewURL,
		Title:       title,
		Description: description,
		SiteName:    siteName,
	}}
}

func summarizePayload(payload map[string]any) string {
	if payload == nil {
		return ""
	}

	ignored := map[string]bool{
		"content": true,
		"text":    true,
		"embeds":  true,
		"embed":   true,
		"items":   true,
		"body":    true,
	}

	keys := make([]string, 0, len(payload))
	for key := range payload {
		if ignored[key] {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, 3)
	for _, key := range keys {
		if len(parts) >= 3 {
			break
		}
		value := scalarToString(payload[key])
		if value == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", key, truncateText(value, 80)))
	}

	return strings.Join(parts, " | ")
}

func sanitizeWebhookName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "Incoming Webhook"
	}
	return truncateText(trimmed, 80)
}

func sanitizeOptionalString(value *string, maxLen int) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	result := truncateText(trimmed, maxLen)
	return &result
}

func sanitizeProviderHint(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(strings.ToLower(*value))
	if trimmed == "" {
		return nil
	}
	result := truncateText(trimmed, 32)
	return &result
}

func generateWebhookToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func hashWebhookToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func tokenPreview(token string) string {
	if len(token) <= 10 {
		return token
	}
	return token[:10]
}

func secureCompareTokens(expectedHash string, actualHash string) bool {
	if len(expectedHash) != len(actualHash) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expectedHash), []byte(actualHash)) == 1
}

func truncateText(value string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	trimmed := strings.TrimSpace(value)
	runes := []rune(trimmed)
	if len(runes) <= maxLen {
		return trimmed
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

func providerDisplayName(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	case "stripe":
		return "Stripe"
	case "slack":
		return "Slack"
	default:
		provider = strings.TrimSpace(provider)
		if provider == "" {
			return "Webhook"
		}
		return strings.ToUpper(provider[:1]) + provider[1:]
	}
}

func safeAction(action string) string {
	action = strings.TrimSpace(action)
	if action == "" {
		return "updated"
	}
	return action
}

func githubPullRequestVerb(action string, merged bool) string {
	action = strings.TrimSpace(strings.ToLower(action))
	switch action {
	case "opened":
		return "opened"
	case "closed":
		if merged {
			return "merged"
		}
		return "closed"
	case "reopened":
		return "reopened"
	case "synchronize", "synchronized":
		return "updated"
	case "ready_for_review":
		return "marked ready for review"
	case "converted_to_draft":
		return "converted to draft"
	default:
		return safeAction(action)
	}
}

func githubIssueVerb(action string) string {
	action = strings.TrimSpace(strings.ToLower(action))
	switch action {
	case "opened":
		return "opened"
	case "closed":
		return "closed"
	case "reopened":
		return "reopened"
	default:
		return safeAction(action)
	}
}

func firstString(source map[string]any, keys ...string) string {
	for _, key := range keys {
		if source == nil {
			return ""
		}
		if value, ok := source[key]; ok {
			if parsed := scalarToString(value); parsed != "" {
				return parsed
			}
		}
	}
	return ""
}

func nestedString(source map[string]any, path ...string) string {
	if source == nil || len(path) == 0 {
		return ""
	}

	var current any = source
	for _, part := range path {
		nextMap, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = nextMap[part]
	}
	return scalarToString(current)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func scalarToString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		f := float64(typed)
		if f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	default:
		return ""
	}
}

func intFromAny(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case int32:
		return int(typed)
	case float64:
		return int(typed)
	case float32:
		return int(typed)
	case json.Number:
		if i, err := typed.Int64(); err == nil {
			return int(i)
		}
		return 0
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0
		}
		return i
	default:
		return 0
	}
}

func asMap(value any) map[string]any {
	converted, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return converted
}

func asSlice(value any) []any {
	switch typed := value.(type) {
	case nil:
		return nil
	case []any:
		return typed
	case []map[string]any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, item)
		}
		return result
	default:
		return nil
	}
}
