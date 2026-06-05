package community

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
	"github.com/zentra/server/internal/services/messaging"
	"github.com/zentra/server/internal/utils"
	"github.com/zentra/server/pkg/auth"
	"github.com/zentra/server/pkg/database"
	"github.com/zentra/server/pkg/encryption"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrCommunityNotFound = errors.New("community not found")
	ErrNotMember         = errors.New("user is not a member of this community")
	ErrAlreadyMember     = errors.New("user is already a member of this community")
	ErrNotOwner          = errors.New("only the owner can perform this action")
	ErrInvalidInvite     = errors.New("invalid or expired invite")
	ErrInsufficientPerms = errors.New("insufficient permissions")
	ErrRoleNotFound      = errors.New("role not found")
	ErrCannotRemoveOwner = errors.New("cannot remove the owner")
	ErrUserBanned        = errors.New("user is banned from this community")
	ErrNotBanned         = errors.New("user is not banned from this community")
	ErrCannotBanOwner    = errors.New("cannot ban the owner")
)

type Service struct {
	db        *pgxpool.Pool
	redis     *redis.Client
	cipher    messaging.ContentCipher
	masterKey []byte
}

func NewService(db *pgxpool.Pool, redis *redis.Client, encryptionKey []byte) *Service {
	return &Service{db: db, redis: redis, cipher: messaging.NewChannelCipher(), masterKey: encryptionKey}
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

type CreateCommunityRequest struct {
	Name        string  `json:"name" validate:"required,min=2,max=100"`
	Description *string `json:"description" validate:"omitempty,max=1000"`
	IsPublic    bool    `json:"isPublic"`
	IsOpen      bool    `json:"isOpen"`
}

type DiscordImportRequest struct {
	OwnerID  uuid.UUID              `json:"ownerId" validate:"required"`
	Guild    DiscordImportGuild     `json:"guild" validate:"required"`
	Channels []DiscordImportChannel `json:"channels" validate:"required,min=1,max=2000"`
	Invite   DiscordInviteOptions   `json:"invite"`
}

type DiscordImportGuild struct {
	Name        string  `json:"name" validate:"required,min=2,max=100"`
	Description *string `json:"description" validate:"omitempty,max=1000"`
	IconURL     *string `json:"iconUrl" validate:"omitempty,url"`
	BannerURL   *string `json:"bannerUrl" validate:"omitempty,url"`
	IsPublic    bool    `json:"isPublic"`
	IsOpen      bool    `json:"isOpen"`
}

type DiscordInviteOptions struct {
	MaxUses   *int   `json:"maxUses" validate:"omitempty,min=1,max=10000"`
	ExpiresIn *int64 `json:"expiresIn" validate:"omitempty,min=60,max=2592000"`
}

type DiscordImportChannel struct {
	SourceID         string                 `json:"sourceId" validate:"required,max=128"`
	Name             string                 `json:"name" validate:"required,max=100"`
	Type             string                 `json:"type" validate:"omitempty,max=32"`
	Topic            *string                `json:"topic" validate:"omitempty,max=1024"`
	CategoryName     *string                `json:"categoryName" validate:"omitempty,max=64"`
	CategoryPosition *int                   `json:"categoryPosition"`
	Position         int                    `json:"position"`
	IsNSFW           bool                   `json:"isNsfw"`
	SlowmodeSeconds  int                    `json:"slowmodeSeconds" validate:"min=0,max=21600"`
	Messages         []DiscordImportMessage `json:"messages"`
}

type DiscordImportMessage struct {
	SourceID        string                    `json:"sourceId" validate:"required,max=128"`
	AuthorName      *string                   `json:"authorName" validate:"omitempty,max=64"`
	AuthorDiscordID *string                   `json:"authorDiscordId" validate:"omitempty,max=64"`
	AuthorAvatarURL *string                   `json:"authorAvatarUrl" validate:"omitempty,url,max=2000"`
	Content         string                    `json:"content" validate:"max=4000"`
	CreatedAt       *time.Time                `json:"createdAt"`
	EditedAt        *time.Time                `json:"editedAt"`
	Pinned          bool                      `json:"pinned"`
	ReplyToSourceID *string                   `json:"replyToSourceId" validate:"omitempty,max=128"`
	Attachments     []DiscordImportAttachment `json:"attachments" validate:"max=32"`
}

type DiscordImportAttachment struct {
	Filename     string  `json:"filename" validate:"required,max=255"`
	URL          string  `json:"url" validate:"required,url,max=2000"`
	Size         int64   `json:"size" validate:"min=0,max=1073741824"`
	ContentType  *string `json:"contentType" validate:"omitempty,max=128"`
	ThumbnailURL *string `json:"thumbnailUrl" validate:"omitempty,url,max=2000"`
	Width        *int    `json:"width"`
	Height       *int    `json:"height"`
}

type DiscordImportResponse struct {
	Community      *models.Community `json:"community"`
	InviteCode     string            `json:"inviteCode"`
	InviteURL      string            `json:"inviteUrl"`
	ImportedCounts struct {
		Channels    int `json:"channels"`
		Messages    int `json:"messages"`
		Attachments int `json:"attachments"`
	} `json:"importedCounts"`
}

func (s *Service) broadcast(ctx context.Context, communityID uuid.UUID, eventType string, data interface{}) {
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
		ChannelID: "", // Global broadcast for now
		Event:     event,
	}

	jsonData, err := json.Marshal(broadcast)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal community update broadcast")
		return
	}

	err = s.redis.Publish(ctx, "websocket:broadcast", jsonData).Err()
	if err != nil {
		log.Error().Err(err).Msg("Failed to publish community update to Redis")
	}
}

func (s *Service) CreateCommunity(ctx context.Context, ownerID uuid.UUID, req *CreateCommunityRequest) (*models.Community, error) {
	community := &models.Community{
		ID:          uuid.New(),
		Name:        req.Name,
		Description: req.Description,
		OwnerID:     ownerID,
		IsPublic:    req.IsPublic,
		IsOpen:      req.IsOpen,
		MemberCount: 0, // Trigger on community_members will increment to 1
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	dek, err := encryption.GenerateDEK()
	if err != nil {
		return nil, err
	}
	wrappedDEK, err := encryption.WrapKey(dek, s.masterKey)
	if err != nil {
		return nil, err
	}

	err = database.WithTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Create community
		_, err := tx.Exec(ctx,
			`INSERT INTO communities (id, name, description, owner_id, is_public, is_open, member_count, encrypted_dek, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			community.ID, community.Name, community.Description, community.OwnerID,
			community.IsPublic, community.IsOpen, community.MemberCount, wrappedDEK, community.CreatedAt, community.UpdatedAt,
		)
		if err != nil {
			return err
		}

		// Add owner as member
		memberID := uuid.New()
		_, err = tx.Exec(ctx,
			`INSERT INTO community_members (id, community_id, user_id, joined_at)
			VALUES ($1, $2, $3, NOW())`,
			memberID, community.ID, ownerID,
		)
		if err != nil {
			return err
		}

		// Create administrator role and assign it to owner
		adminRoleID := uuid.New()
		_, err = tx.Exec(ctx,
			`INSERT INTO roles (id, community_id, name, permissions, is_default, position)
			VALUES ($1, $2, 'Administrator', $3, FALSE, 100)`,
			adminRoleID, community.ID, models.PermissionAllAdmin,
		)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO member_roles (member_id, role_id) VALUES ($1, $2)`,
			memberID, adminRoleID,
		)
		if err != nil {
			return err
		}

		// Create default role
		_, err = tx.Exec(ctx,
			`INSERT INTO roles (id, community_id, name, permissions, is_default, position)
			VALUES ($1, $2, 'Member', $3, TRUE, 0)`,
			uuid.New(), community.ID, models.PermissionAllText,
		)
		if err != nil {
			return err
		}

		// Create default general channel
		_, err = tx.Exec(ctx,
			`INSERT INTO channels (id, community_id, name, type, position)
			VALUES ($1, $2, 'general', 'text', 0)`,
			uuid.New(), community.ID,
		)
		if err != nil {
			return err
		}

		// Create audit log
		details, _ := json.Marshal(map[string]string{"name": community.Name})
		_, err = tx.Exec(ctx,
			`INSERT INTO audit_logs (id, community_id, actor_id, action, target_type, target_id, details)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.New(), community.ID, ownerID, models.AuditActionCommunityCreate, "community", community.ID, details,
		)
		return err
	})

	if err != nil {
		return nil, err
	}

	return community, nil
}

func (s *Service) ImportDiscordServer(ctx context.Context, req *DiscordImportRequest) (*DiscordImportResponse, error) {
	if req == nil {
		return nil, errors.New("request is required")
	}

	if s.cipher == nil {
		return nil, errors.New("message cipher is not configured")
	}

	if err := utils.Validate(req); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	community := &models.Community{
		ID:          uuid.New(),
		Name:        req.Guild.Name,
		Description: req.Guild.Description,
		IconURL:     req.Guild.IconURL,
		BannerURL:   req.Guild.BannerURL,
		OwnerID:     req.OwnerID,
		IsPublic:    req.Guild.IsPublic,
		IsOpen:      req.Guild.IsOpen,
		MemberCount: 0,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	inviteCode, err := auth.GenerateInviteCode()
	if err != nil {
		return nil, err
	}

	importDEK, err := encryption.GenerateDEK()
	if err != nil {
		return nil, err
	}
	wrappedDEK, err := encryption.WrapKey(importDEK, s.masterKey)
	if err != nil {
		return nil, err
	}

	response := &DiscordImportResponse{Community: community}
	response.InviteCode = inviteCode
	response.InviteURL = "/api/v1/communities/invite/" + inviteCode

	err = database.WithTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO communities (id, name, description, icon_url, banner_url, owner_id, is_public, is_open, member_count, encrypted_dek, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			community.ID, community.Name, community.Description, community.IconURL, community.BannerURL, community.OwnerID,
			community.IsPublic, community.IsOpen, community.MemberCount, wrappedDEK, community.CreatedAt, community.UpdatedAt,
		)
		if err != nil {
			return err
		}

		memberID := uuid.New()
		_, err = tx.Exec(ctx,
			`INSERT INTO community_members (id, community_id, user_id, joined_at)
			VALUES ($1, $2, $3, NOW())`,
			memberID, community.ID, req.OwnerID,
		)
		if err != nil {
			return err
		}

		adminRoleID := uuid.New()
		_, err = tx.Exec(ctx,
			`INSERT INTO roles (id, community_id, name, permissions, is_default, position)
			VALUES ($1, $2, 'Administrator', $3, FALSE, 100)`,
			adminRoleID, community.ID, models.PermissionAllAdmin,
		)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO member_roles (member_id, role_id) VALUES ($1, $2)`,
			memberID, adminRoleID,
		)
		if err != nil {
			return err
		}

		defaultRoleID := uuid.New()
		_, err = tx.Exec(ctx,
			`INSERT INTO roles (id, community_id, name, permissions, is_default, position)
			VALUES ($1, $2, 'Member', $3, TRUE, 0)`,
			defaultRoleID, community.ID, models.PermissionAllText,
		)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO member_roles (member_id, role_id) VALUES ($1, $2)`,
			memberID, defaultRoleID,
		)
		if err != nil {
			return err
		}

		categoryByName := make(map[string]uuid.UUID)
		categoryPosByName := make(map[string]int)

		for _, importedChannel := range req.Channels {
			if importedChannel.CategoryName == nil || strings.TrimSpace(*importedChannel.CategoryName) == "" {
				continue
			}

			categoryName := strings.TrimSpace(*importedChannel.CategoryName)
			if _, exists := categoryByName[categoryName]; exists {
				continue
			}

			categoryPosition := 0
			if importedChannel.CategoryPosition != nil {
				categoryPosition = *importedChannel.CategoryPosition
			}

			categoryID := uuid.New()
			_, err = tx.Exec(ctx,
				`INSERT INTO channel_categories (id, community_id, name, position, created_at)
				VALUES ($1, $2, $3, $4, $5)`,
				categoryID, community.ID, categoryName, categoryPosition, now,
			)
			if err != nil {
				return err
			}

			categoryByName[categoryName] = categoryID
			categoryPosByName[categoryName] = categoryPosition
		}

		createdMessageBySource := make(map[string]uuid.UUID)
		lastMessageAtByChannel := make(map[uuid.UUID]time.Time)
		authorUserIDByKey := make(map[string]uuid.UUID)
		memberIDByUserID := map[uuid.UUID]uuid.UUID{req.OwnerID: memberID}

		for _, importedChannel := range req.Channels {
			channelID := uuid.New()
			channelType := normalizeImportedChannelType(importedChannel.Type)
			channelName := utils.NormalizeChannelName(importedChannel.Name)
			if channelName == "" {
				channelName = fmt.Sprintf("channel-%s", channelID.String()[:8])
			}

			var categoryID *uuid.UUID
			if importedChannel.CategoryName != nil {
				if catID, ok := categoryByName[strings.TrimSpace(*importedChannel.CategoryName)]; ok {
					categoryID = &catID
				}
			}

			_, err = tx.Exec(ctx,
				`INSERT INTO channels (id, community_id, category_id, name, topic, type, position, is_nsfw, slowmode_seconds, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				channelID, community.ID, categoryID, channelName, importedChannel.Topic, channelType,
				importedChannel.Position, importedChannel.IsNSFW, importedChannel.SlowmodeSeconds, now, now,
			)
			if err != nil {
				return err
			}

			response.ImportedCounts.Channels++

			for messageIndex, importedMessage := range importedChannel.Messages {
				createdAt := now.Add(time.Duration(response.ImportedCounts.Messages+messageIndex) * time.Millisecond)
				if importedMessage.CreatedAt != nil && !importedMessage.CreatedAt.IsZero() {
					createdAt = importedMessage.CreatedAt.UTC()
				}

				authorID := req.OwnerID
				authorKey := importedAuthorKey(importedMessage.AuthorDiscordID, importedMessage.AuthorName)
				if authorKey != "" {
					if cachedAuthorID, ok := authorUserIDByKey[authorKey]; ok {
						authorID = cachedAuthorID
					} else {
						authorName := importedAuthorName(importedMessage.AuthorName)
						avatarURL := importedMessage.AuthorAvatarURL
						createdAuthorID, err := ensureImportedAuthorUser(ctx, tx, community.ID, authorKey, authorName, avatarURL)
						if err != nil {
							return err
						}
						authorUserIDByKey[authorKey] = createdAuthorID
						authorID = createdAuthorID
					}
				}

				if _, exists := memberIDByUserID[authorID]; !exists {
					importedMemberID := uuid.New()
					_, err = tx.Exec(ctx,
						`INSERT INTO community_members (id, community_id, user_id, joined_at)
						VALUES ($1, $2, $3, NOW())
						ON CONFLICT (community_id, user_id) DO NOTHING`,
						importedMemberID, community.ID, authorID,
					)
					if err != nil {
						return err
					}
					_, err = tx.Exec(ctx,
						`INSERT INTO member_roles (member_id, role_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
						importedMemberID, defaultRoleID,
					)
					if err != nil {
						return err
					}
					memberIDByUserID[authorID] = importedMemberID
				}

				importedContent := importedMessage.Content

				encryptedContent, _, err := s.cipher.Encrypt(importedContent, importDEK)
				if err != nil {
					return err
				}

				messageID := uuid.New()
				var replyToID *uuid.UUID
				if importedMessage.ReplyToSourceID != nil {
					if mappedReplyID, ok := createdMessageBySource[*importedMessage.ReplyToSourceID]; ok {
						replyToID = &mappedReplyID
					}
				}

				isEdited := importedMessage.EditedAt != nil
				updatedAt := createdAt
				if importedMessage.EditedAt != nil && !importedMessage.EditedAt.IsZero() {
					updatedAt = importedMessage.EditedAt.UTC()
				}
				_, err = tx.Exec(ctx,
					`INSERT INTO messages (id, channel_id, author_id, encrypted_content, reply_to_id, is_edited, is_pinned, reactions, link_previews, created_at, updated_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, '{}'::jsonb, '[]'::jsonb, $8, $9)`,
					messageID, channelID, authorID, encryptedContent, replyToID, isEdited, importedMessage.Pinned, createdAt, updatedAt,
				)
				if err != nil {
					return err
				}

				if importedMessage.SourceID != "" {
					createdMessageBySource[importedMessage.SourceID] = messageID
				}
				lastMessageAtByChannel[channelID] = createdAt
				response.ImportedCounts.Messages++

				for _, attachment := range importedMessage.Attachments {
					attachmentID := uuid.New()
					_, err = tx.Exec(ctx,
						`INSERT INTO message_attachments (
							id, message_id, uploader_id, filename, file_url,
							file_size, content_type, thumbnail_url, width, height, created_at
						)
						VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
						attachmentID, messageID, authorID, attachment.Filename,
						attachment.URL, attachment.Size, attachment.ContentType, attachment.ThumbnailURL,
						attachment.Width, attachment.Height, createdAt,
					)
					if err != nil {
						return err
					}
					response.ImportedCounts.Attachments++
				}
			}
		}

		for channelID, lastMessageAt := range lastMessageAtByChannel {
			_, err = tx.Exec(ctx,
				`UPDATE channels SET last_message_at = $2, updated_at = NOW() WHERE id = $1`,
				channelID, lastMessageAt,
			)
			if err != nil {
				return err
			}
		}

		var inviteExpiresAt *time.Time
		if req.Invite.ExpiresIn != nil {
			expires := now.Add(time.Duration(*req.Invite.ExpiresIn) * time.Second)
			inviteExpiresAt = &expires
		}

		inviteID := uuid.New()
		_, err = tx.Exec(ctx,
			`INSERT INTO community_invites (id, community_id, code, created_by, max_uses, use_count, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, 0, $6, $7)`,
			inviteID, community.ID, inviteCode, req.OwnerID, req.Invite.MaxUses, inviteExpiresAt, now,
		)
		if err != nil {
			return err
		}

		details, _ := json.Marshal(map[string]any{
			"source":      "discord",
			"channels":    response.ImportedCounts.Channels,
			"messages":    response.ImportedCounts.Messages,
			"attachments": response.ImportedCounts.Attachments,
		})
		_, err = tx.Exec(ctx,
			`INSERT INTO audit_logs (id, community_id, actor_id, action, target_type, target_id, details)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.New(), community.ID, req.OwnerID, "community.discord_import", "community", community.ID, details,
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	s.broadcast(ctx, community.ID, "COMMUNITY_CREATE", community)
	return response, nil
}

var importedUsernameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func importedAuthorKey(discordID *string, authorName *string) string {
	if discordID != nil {
		trimmed := strings.TrimSpace(*discordID)
		if trimmed != "" {
			return "discord:" + trimmed
		}
	}
	if authorName != nil {
		trimmed := strings.TrimSpace(strings.ToLower(*authorName))
		if trimmed != "" {
			return "name:" + trimmed
		}
	}
	return ""
}

func importedAuthorName(authorName *string) string {
	if authorName == nil {
		return "discord-user"
	}
	trimmed := strings.TrimSpace(*authorName)
	if trimmed == "" {
		return "discord-user"
	}
	return trimmed
}

func importedUsernameFromName(name string, suffix string) string {
	base := importedUsernameSanitizer.ReplaceAllString(name, "")
	base = strings.TrimSpace(base)
	if base == "" {
		base = "discorduser"
	}
	if len(base) > 20 {
		base = base[:20]
	}
	return strings.ToLower(base + "_" + suffix)
}

func ensureImportedAuthorUser(ctx context.Context, tx pgx.Tx, communityID uuid.UUID, authorKey string, authorName string, authorAvatarURL *string) (uuid.UUID, error) {
	hash := sha1.Sum([]byte(communityID.String() + ":" + authorKey))
	seed := fmt.Sprintf("%x", hash[:])
	passwordHashBytes, err := bcrypt.GenerateFromPassword([]byte(seed), bcrypt.MinCost)
	if err != nil {
		return uuid.Nil, err
	}

	suffix := seed[:10]
	username := importedUsernameFromName(authorName, suffix)
	email := fmt.Sprintf("discord-import+%s@zentra.import", suffix)
	userID := uuid.New()

	var ensuredUserID uuid.UUID
	err = tx.QueryRow(ctx,
		`INSERT INTO users (id, username, email, password_hash, display_name, avatar_url, status, email_verified, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'offline', TRUE, NOW(), NOW())
		ON CONFLICT (email) DO UPDATE
		SET display_name = EXCLUDED.display_name,
			avatar_url = COALESCE(EXCLUDED.avatar_url, users.avatar_url),
			updated_at = NOW()
		RETURNING id`,
		userID, username, email, string(passwordHashBytes), authorName, authorAvatarURL,
	).Scan(&ensuredUserID)
	if err == nil {
		return ensuredUserID, nil
	}

	// If username collided for a different email, resolve with a fallback username suffix.
	fallbackUsername := importedUsernameFromName(authorName, seed[10:20])
	err = tx.QueryRow(ctx,
		`INSERT INTO users (id, username, email, password_hash, display_name, avatar_url, status, email_verified, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'offline', TRUE, NOW(), NOW())
		ON CONFLICT (email) DO UPDATE
		SET display_name = EXCLUDED.display_name,
			avatar_url = COALESCE(EXCLUDED.avatar_url, users.avatar_url),
			updated_at = NOW()
		RETURNING id`,
		uuid.New(), fallbackUsername, email, string(passwordHashBytes), authorName, authorAvatarURL,
	).Scan(&ensuredUserID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to ensure imported author user for key %s: %w", authorKey, err)
	}

	return ensuredUserID, nil
}

func normalizeImportedChannelType(importedType string) models.ChannelType {
	switch strings.ToLower(strings.TrimSpace(importedType)) {
	case "announcement", "news":
		return models.ChannelTypeAnnouncement
	case "gallery", "media":
		return models.ChannelTypeGallery
	case "forum":
		return models.ChannelTypeForum
	default:
		return models.ChannelTypeText
	}
}

func (s *Service) GetCommunity(ctx context.Context, id uuid.UUID) (*models.Community, error) {
	community := &models.Community{}
	err := s.db.QueryRow(ctx,
		`SELECT id, name, description, icon_url, banner_url, owner_id, is_public, is_open, member_count, created_at, updated_at
		FROM communities WHERE id = $1 AND deleted_at IS NULL`,
		id,
	).Scan(
		&community.ID, &community.Name, &community.Description, &community.IconURL,
		&community.BannerURL, &community.OwnerID, &community.IsPublic, &community.IsOpen,
		&community.MemberCount, &community.CreatedAt, &community.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCommunityNotFound
		}
		return nil, err
	}
	return community, nil
}

func (s *Service) GetUserCommunities(ctx context.Context, userID uuid.UUID) ([]*models.Community, error) {
	// Fetch the user's community layout order
	var layoutRaw json.RawMessage
	err := s.db.QueryRow(ctx,
		`SELECT COALESCE(community_layout, '{"communityOrder":[]}'::jsonb) FROM users WHERE id = $1`,
		userID,
	).Scan(&layoutRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	var layout struct {
		CommunityOrder []uuid.UUID `json:"communityOrder"`
	}
	if err := json.Unmarshal(layoutRaw, &layout); err != nil {
		log.Warn().Err(err).Msg("Failed to parse community_layout, falling back to name sort")
	}

	orderIndex := make(map[uuid.UUID]int, len(layout.CommunityOrder))
	for i, id := range layout.CommunityOrder {
		orderIndex[id] = i
	}

	rows, err := s.db.Query(ctx,
		`SELECT c.id, c.name, c.description, c.icon_url, c.banner_url, c.owner_id, 
		c.is_public, c.is_open, c.member_count, c.created_at, c.updated_at
		FROM communities c
		JOIN community_members cm ON cm.community_id = c.id
		WHERE cm.user_id = $1 AND c.deleted_at IS NULL`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	communities := make([]*models.Community, 0, len(layout.CommunityOrder))
	unordered := make([]*models.Community, 0)
	for rows.Next() {
		c := &models.Community{}
		err := rows.Scan(
			&c.ID, &c.Name, &c.Description, &c.IconURL, &c.BannerURL,
			&c.OwnerID, &c.IsPublic, &c.IsOpen, &c.MemberCount, &c.CreatedAt, &c.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		if _, ok := orderIndex[c.ID]; ok {
			communities = append(communities, c)
		} else {
			unordered = append(unordered, c)
		}
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate user communities rows")
		return nil, err
	}

	// Sort by layout order, append any new communities at the end alphabetically
	sort.Slice(communities, func(i, j int) bool {
		return orderIndex[communities[i].ID] < orderIndex[communities[j].ID]
	})
	sort.Slice(unordered, func(i, j int) bool {
		return unordered[i].Name < unordered[j].Name
	})

	communities = append(communities, unordered...)

	return communities, nil
}

func (s *Service) DiscoverCommunities(ctx context.Context, query string, limit, offset int) ([]*models.Community, int64, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	var total int64
	baseQuery := `WHERE is_public = TRUE AND deleted_at IS NULL`
	args := []interface{}{}

	if query != "" {
		query = utils.SanitizeSearchQuery(query)
		baseQuery += ` AND (name ILIKE $1 OR description ILIKE $1)`
		args = append(args, "%"+query+"%")
	}

	countQuery := `SELECT COUNT(*) FROM communities ` + baseQuery
	err := s.db.QueryRow(ctx, countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	selectQuery := `SELECT id, name, description, icon_url, banner_url, owner_id, is_public, is_open, member_count, created_at, updated_at
		FROM communities ` + baseQuery + ` ORDER BY member_count DESC LIMIT $` + string(rune('0'+len(args)+1)) + ` OFFSET $` + string(rune('0'+len(args)+2))
	args = append(args, limit, offset)

	rows, err := s.db.Query(ctx, selectQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var communities []*models.Community
	for rows.Next() {
		c := &models.Community{}
		err := rows.Scan(
			&c.ID, &c.Name, &c.Description, &c.IconURL, &c.BannerURL,
			&c.OwnerID, &c.IsPublic, &c.IsOpen, &c.MemberCount, &c.CreatedAt, &c.UpdatedAt,
		)
		if err != nil {
			return nil, 0, err
		}
		communities = append(communities, c)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate discover communities rows")
		return nil, 0, err
	}

	return communities, total, nil
}

type UpdateCommunityRequest struct {
	Name        *string `json:"name" validate:"omitempty,min=2,max=100"`
	Description *string `json:"description" validate:"omitempty,max=1000"`
	IsPublic    *bool   `json:"isPublic"`
	IsOpen      *bool   `json:"isOpen"`
}

func (s *Service) UpdateCommunity(ctx context.Context, communityID, userID uuid.UUID, req *UpdateCommunityRequest) (*models.Community, error) {
	// Check permissions
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionManageCommunity); err != nil {
		return nil, err
	}

	_, err := s.db.Exec(ctx,
		`UPDATE communities SET 
			name = COALESCE($2, name),
			description = COALESCE($3, description),
			is_public = COALESCE($4, is_public),
			is_open = COALESCE($5, is_open),
			updated_at = NOW()
		WHERE id = $1`,
		communityID, req.Name, req.Description, req.IsPublic, req.IsOpen,
	)
	if err != nil {
		return nil, err
	}

	community, err := s.GetCommunity(ctx, communityID)
	if err == nil {
		s.broadcast(ctx, communityID, "COMMUNITY_UPDATE", community)
	}

	// Log what changed
	changes := map[string]interface{}{}
	if req.Name != nil {
		changes["name"] = *req.Name
	}
	if req.Description != nil {
		changes["description"] = *req.Description
	}
	if req.IsPublic != nil {
		changes["isPublic"] = *req.IsPublic
	}
	if req.IsOpen != nil {
		changes["isOpen"] = *req.IsOpen
	}
	if len(changes) > 0 {
		details, _ := json.Marshal(changes)
		s.LogAudit(ctx, &communityID, userID, models.AuditActionCommunityUpdate, "community", &communityID, details)
	}

	return community, err
}

func (s *Service) UpdateCommunityIcon(ctx context.Context, communityID, userID uuid.UUID, iconURL string) error {
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionManageCommunity); err != nil {
		return err
	}

	_, err := s.db.Exec(ctx,
		`UPDATE communities SET icon_url = $2, updated_at = NOW() WHERE id = $1`,
		communityID, iconURL,
	)
	if err == nil {
		if community, err := s.GetCommunity(ctx, communityID); err == nil {
			s.broadcast(ctx, communityID, "COMMUNITY_UPDATE", community)
		}
		details, _ := json.Marshal(map[string]string{"field": "icon"})
		s.LogAudit(ctx, &communityID, userID, models.AuditActionCommunityUpdate, "community", &communityID, details)
	}
	return err
}

func (s *Service) UpdateCommunityBanner(ctx context.Context, communityID, userID uuid.UUID, bannerURL string) error {
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionManageCommunity); err != nil {
		return err
	}

	_, err := s.db.Exec(ctx,
		`UPDATE communities SET banner_url = $2, updated_at = NOW() WHERE id = $1`,
		communityID, bannerURL,
	)
	if err == nil {
		if community, err := s.GetCommunity(ctx, communityID); err == nil {
			s.broadcast(ctx, communityID, "COMMUNITY_UPDATE", community)
		}
	}
	return err
}

func (s *Service) RemoveCommunityIcon(ctx context.Context, communityID, userID uuid.UUID) error {
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionManageCommunity); err != nil {
		return err
	}

	_, err := s.db.Exec(ctx,
		`UPDATE communities SET icon_url = NULL, updated_at = NOW() WHERE id = $1`,
		communityID,
	)
	if err == nil {
		if community, err := s.GetCommunity(ctx, communityID); err == nil {
			s.broadcast(ctx, communityID, "COMMUNITY_UPDATE", community)
		}
		details, _ := json.Marshal(map[string]string{"field": "icon", "action": "removed"})
		s.LogAudit(ctx, &communityID, userID, models.AuditActionCommunityUpdate, "community", &communityID, details)
	}
	return err
}

func (s *Service) RemoveCommunityBanner(ctx context.Context, communityID, userID uuid.UUID) error {
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionManageCommunity); err != nil {
		return err
	}

	_, err := s.db.Exec(ctx,
		`UPDATE communities SET banner_url = NULL, updated_at = NOW() WHERE id = $1`,
		communityID,
	)
	if err == nil {
		if community, err := s.GetCommunity(ctx, communityID); err == nil {
			s.broadcast(ctx, communityID, "COMMUNITY_UPDATE", community)
		}
	}
	return err
}

func (s *Service) DeleteCommunity(ctx context.Context, communityID, userID uuid.UUID) error {
	// Only owner can delete
	community, err := s.GetCommunity(ctx, communityID)
	if err != nil {
		return err
	}
	if community.OwnerID != userID {
		return ErrNotOwner
	}

	_, err = s.db.Exec(ctx,
		`UPDATE communities SET deleted_at = NOW() WHERE id = $1`,
		communityID,
	)
	if err == nil {
		details, _ := json.Marshal(map[string]string{"name": community.Name})
		s.LogAudit(ctx, &communityID, userID, models.AuditActionCommunityDelete, "community", &communityID, details)
	}
	return err
}

// Member Management

func (s *Service) GetMember(ctx context.Context, communityID, userID uuid.UUID) (*models.CommunityMember, error) {
	member := &models.CommunityMember{}
	err := s.db.QueryRow(ctx,
		`SELECT id, community_id, user_id, nickname, joined_at
		FROM community_members WHERE community_id = $1 AND user_id = $2`,
		communityID, userID,
	).Scan(&member.ID, &member.CommunityID, &member.UserID, &member.Nickname, &member.JoinedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotMember
		}
		return nil, err
	}
	return member, nil
}

func (s *Service) GetMembers(ctx context.Context, communityID uuid.UUID, limit, offset int) ([]*models.CommunityMemberWithUser, int64, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	var total int64
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM community_members WHERE community_id = $1`,
		communityID,
	).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT cm.id, cm.community_id, cm.user_id, cm.nickname, cm.joined_at,
		u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		FROM community_members cm
		JOIN users u ON u.id = cm.user_id
		WHERE cm.community_id = $1
		ORDER BY cm.joined_at
		LIMIT $2 OFFSET $3`,
		communityID, limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var members []*models.CommunityMemberWithUser
	for rows.Next() {
		m := &models.CommunityMemberWithUser{}
		u := &models.PublicUser{}
		err := rows.Scan(
			&m.ID, &m.CommunityID, &m.UserID, &m.Nickname, &m.JoinedAt,
			&u.ID, &u.Username, &u.DisplayName, &u.AvatarURL, &u.Bio, &u.Status, &u.CustomStatus, &u.CreatedAt,
		)
		if err != nil {
			return nil, 0, err
		}
		m.User = u
		members = append(members, m)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate member rows")
		return nil, 0, err
	}

	if len(members) > 0 {
		memberIDs := make([]uuid.UUID, 0, len(members))
		memberByID := make(map[uuid.UUID]*models.CommunityMemberWithUser, len(members))
		for _, member := range members {
			memberIDs = append(memberIDs, member.ID)
			memberByID[member.ID] = member
		}

		rows, err := s.db.Query(ctx,
			`SELECT mr.member_id, r.id, r.community_id, r.name, r.color, r.position, r.permissions, r.is_default, r.created_at, r.updated_at
			FROM member_roles mr
			JOIN roles r ON r.id = mr.role_id
			WHERE mr.member_id = ANY($1)
			ORDER BY r.position DESC`,
			memberIDs,
		)
		if err != nil {
			return nil, 0, err
		}
		defer rows.Close()

		for rows.Next() {
			var memberID uuid.UUID
			r := &models.Role{}
			err := rows.Scan(
				&memberID, &r.ID, &r.CommunityID, &r.Name, &r.Color, &r.Position,
				&r.Permissions, &r.IsDefault, &r.CreatedAt, &r.UpdatedAt,
			)
			if err != nil {
				return nil, 0, err
			}
			if member, ok := memberByID[memberID]; ok {
				member.Roles = append(member.Roles, r)
			}
		}

		if err := rows.Err(); err != nil {
			log.Warn().Err(err).Msg("Failed to iterate member roles rows")
			return nil, 0, err
		}

		defaultRole, err := s.GetDefaultRole(ctx, communityID)
		if err == nil && defaultRole != nil {
			for _, member := range members {
				if len(member.Roles) == 0 {
					member.Roles = []*models.Role{defaultRole}
				}
			}
		}
	}

	return members, total, nil
}

func (s *Service) JoinCommunity(ctx context.Context, communityID, userID uuid.UUID) error {
	community, err := s.GetCommunity(ctx, communityID)
	if err != nil {
		return err
	}

	// Check if community is open for direct joins
	if !community.IsOpen && !community.IsPublic {
		return errors.New("this community requires an invite to join")
	}

	// Don't let banned users back in
	if s.IsUserBanned(ctx, communityID, userID) {
		return ErrUserBanned
	}

	if err := s.addMember(ctx, communityID, userID); err != nil {
		return err
	}

	s.LogAudit(ctx, &communityID, userID, models.AuditActionMemberJoin, "user", &userID, nil)
	return nil
}

func (s *Service) JoinWithInvite(ctx context.Context, code string, userID uuid.UUID) (*models.Community, error) {
	// Find and validate invite
	var invite models.CommunityInvite
	err := s.db.QueryRow(ctx,
		`SELECT id, community_id, max_uses, use_count, expires_at
		FROM community_invites WHERE code = $1`,
		code,
	).Scan(&invite.ID, &invite.CommunityID, &invite.MaxUses, &invite.UseCount, &invite.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidInvite
		}
		return nil, err
	}

	// Check expiration
	if invite.ExpiresAt != nil && invite.ExpiresAt.Before(time.Now()) {
		return nil, ErrInvalidInvite
	}

	// Check max uses
	if invite.MaxUses != nil && invite.UseCount >= *invite.MaxUses {
		return nil, ErrInvalidInvite
	}

	// Don't let banned users back in via invite either
	if s.IsUserBanned(ctx, invite.CommunityID, userID) {
		return nil, ErrUserBanned
	}

	// Add member
	if err := s.addMember(ctx, invite.CommunityID, userID); err != nil {
		return nil, err
	}

	s.LogAudit(ctx, &invite.CommunityID, userID, models.AuditActionMemberJoin, "user", &userID, nil)

	// Increment use count
	_, err = s.db.Exec(ctx,
		`UPDATE community_invites SET use_count = use_count + 1 WHERE id = $1`,
		invite.ID,
	)
	if err != nil {
		return nil, err
	}

	return s.GetCommunity(ctx, invite.CommunityID)
}

func (s *Service) addMember(ctx context.Context, communityID, userID uuid.UUID) error {
	// Check if already a member
	_, err := s.GetMember(ctx, communityID, userID)
	if err == nil {
		return ErrAlreadyMember
	}
	if err != ErrNotMember {
		return err
	}

	memberID := uuid.New()
	return database.WithTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO community_members (id, community_id, user_id, joined_at)
			VALUES ($1, $2, $3, NOW())`,
			memberID, communityID, userID,
		)
		if err != nil {
			return err
		}

		var defaultRoleID uuid.UUID
		err = tx.QueryRow(ctx,
			`SELECT id FROM roles WHERE community_id = $1 AND is_default = TRUE`,
			communityID,
		).Scan(&defaultRoleID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO member_roles (member_id, role_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING`,
			memberID, defaultRoleID,
		)
		return err
	})
}

func (s *Service) LeaveCommunity(ctx context.Context, communityID, userID uuid.UUID) error {
	community, err := s.GetCommunity(ctx, communityID)
	if err != nil {
		return err
	}

	// Owner cannot leave (must transfer or delete)
	if community.OwnerID == userID {
		return errors.New("owner cannot leave the community, transfer ownership or delete it")
	}

	_, err = s.db.Exec(ctx,
		`DELETE FROM community_members WHERE community_id = $1 AND user_id = $2`,
		communityID, userID,
	)
	if err == nil {
		s.LogAudit(ctx, &communityID, userID, models.AuditActionMemberLeave, "user", &userID, nil)
	}
	return err
}

func (s *Service) KickMember(ctx context.Context, communityID, actorID, targetID uuid.UUID) error {
	if err := s.requirePermission(ctx, communityID, actorID, models.PermissionKickMembers); err != nil {
		return err
	}

	community, err := s.GetCommunity(ctx, communityID)
	if err != nil {
		return err
	}
	if community.OwnerID == targetID {
		return ErrCannotRemoveOwner
	}

	_, err = s.db.Exec(ctx,
		`DELETE FROM community_members WHERE community_id = $1 AND user_id = $2`,
		communityID, targetID,
	)
	if err != nil {
		return err
	}

	// Log to audit trail
	s.LogAudit(ctx, &communityID, actorID, models.AuditActionMemberKick, "user", &targetID, nil)

	return nil
}

// Ban Management

type BanMemberRequest struct {
	Reason *string `json:"reason" validate:"omitempty,max=512"`
}

func (s *Service) BanMember(ctx context.Context, communityID, actorID, targetID uuid.UUID, reason *string) error {
	if err := s.requirePermission(ctx, communityID, actorID, models.PermissionBanMembers); err != nil {
		return err
	}

	community, err := s.GetCommunity(ctx, communityID)
	if err != nil {
		return err
	}
	if community.OwnerID == targetID {
		return ErrCannotBanOwner
	}

	return database.WithTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Remove from members if they're currently in the community
		_, _ = tx.Exec(ctx,
			`DELETE FROM community_members WHERE community_id = $1 AND user_id = $2`,
			communityID, targetID,
		)

		// Insert the ban record
		_, err := tx.Exec(ctx,
			`INSERT INTO community_bans (id, community_id, user_id, banned_by, reason, created_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (community_id, user_id) DO NOTHING`,
			uuid.New(), communityID, targetID, actorID, reason,
		)
		if err != nil {
			return err
		}

		// Write audit log
		details, _ := json.Marshal(map[string]interface{}{"reason": reason})
		s.LogAudit(ctx, &communityID, actorID, models.AuditActionMemberBan, "user", &targetID, details)

		return nil
	})
}

func (s *Service) UnbanMember(ctx context.Context, communityID, actorID, targetID uuid.UUID) error {
	if err := s.requirePermission(ctx, communityID, actorID, models.PermissionBanMembers); err != nil {
		return err
	}

	result, err := s.db.Exec(ctx,
		`DELETE FROM community_bans WHERE community_id = $1 AND user_id = $2`,
		communityID, targetID,
	)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotBanned
	}

	s.LogAudit(ctx, &communityID, actorID, models.AuditActionMemberUnban, "user", &targetID, nil)

	return nil
}

func (s *Service) GetBans(ctx context.Context, communityID, actorID uuid.UUID) ([]*models.CommunityBanWithUser, error) {
	if err := s.requirePermission(ctx, communityID, actorID, models.PermissionBanMembers); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT cb.id, cb.community_id, cb.user_id, cb.banned_by, cb.reason, cb.created_at,
			u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at,
			b.id, b.username, b.display_name, b.avatar_url, b.bio, b.status, b.custom_status, b.created_at
		FROM community_bans cb
		JOIN users u ON u.id = cb.user_id
		JOIN users b ON b.id = cb.banned_by
		WHERE cb.community_id = $1
		ORDER BY cb.created_at DESC`,
		communityID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bans []*models.CommunityBanWithUser
	for rows.Next() {
		ban := &models.CommunityBanWithUser{}
		user := &models.PublicUser{}
		bannedByUser := &models.PublicUser{}
		err := rows.Scan(
			&ban.ID, &ban.CommunityID, &ban.UserID, &ban.BannedBy, &ban.Reason, &ban.CreatedAt,
			&user.ID, &user.Username, &user.DisplayName, &user.AvatarURL, &user.Bio, &user.Status, &user.CustomStatus, &user.CreatedAt,
			&bannedByUser.ID, &bannedByUser.Username, &bannedByUser.DisplayName, &bannedByUser.AvatarURL, &bannedByUser.Bio, &bannedByUser.Status, &bannedByUser.CustomStatus, &bannedByUser.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		ban.User = user
		ban.BannedByUser = bannedByUser
		bans = append(bans, ban)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate ban rows")
		return nil, err
	}

	return bans, nil
}

func (s *Service) IsUserBanned(ctx context.Context, communityID, userID uuid.UUID) bool {
	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM community_bans WHERE community_id = $1 AND user_id = $2)`,
		communityID, userID,
	).Scan(&exists)
	if err != nil {
		return false
	}
	return exists
}

// Audit Log

func (s *Service) GetAuditLogs(ctx context.Context, communityID, actorID uuid.UUID, limit, offset int) ([]*models.AuditLogWithActor, int64, error) {
	if err := s.requirePermission(ctx, communityID, actorID, models.PermissionViewAuditLog); err != nil {
		return nil, 0, err
	}

	if limit <= 0 || limit > 100 {
		limit = 50
	}

	var total int64
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE community_id = $1`,
		communityID,
	).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT al.id, al.community_id, al.actor_id, al.action, al.target_type, al.target_id, al.details, al.created_at,
			u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		FROM audit_logs al
		JOIN users u ON u.id = al.actor_id
		WHERE al.community_id = $1
		ORDER BY al.created_at DESC
		LIMIT $2 OFFSET $3`,
		communityID, limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var logs []*models.AuditLogWithActor
	for rows.Next() {
		entry := &models.AuditLogWithActor{}
		actor := &models.PublicUser{}
		err := rows.Scan(
			&entry.ID, &entry.CommunityID, &entry.ActorID, &entry.Action,
			&entry.TargetType, &entry.TargetID, &entry.Details, &entry.CreatedAt,
			&actor.ID, &actor.Username, &actor.DisplayName, &actor.AvatarURL,
			&actor.Bio, &actor.Status, &actor.CustomStatus, &actor.CreatedAt,
		)
		if err != nil {
			return nil, 0, err
		}
		entry.Actor = actor
		logs = append(logs, entry)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate audit log rows")
		return nil, 0, err
	}

	return logs, total, nil
}

func (s *Service) LogAudit(ctx context.Context, communityID *uuid.UUID, actorID uuid.UUID, action string, targetType string, targetID *uuid.UUID, details []byte) {
	_, err := s.db.Exec(ctx,
		`INSERT INTO audit_logs (id, community_id, actor_id, action, target_type, target_id, details, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())`,
		uuid.New(), communityID, actorID, action, targetType, targetID, details,
	)
	if err != nil {
		log.Error().Err(err).Str("action", action).Msg("Failed to write audit log")
	}
}

// Invites

func (s *Service) CreateInvite(ctx context.Context, communityID, userID uuid.UUID, maxUses *int, expiresIn *time.Duration) (*models.CommunityInvite, error) {
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionCreateInvites); err != nil {
		return nil, err
	}

	code, err := auth.GenerateInviteCode()
	if err != nil {
		return nil, err
	}

	invite := &models.CommunityInvite{
		ID:          uuid.New(),
		CommunityID: communityID,
		Code:        code,
		CreatedBy:   userID,
		MaxUses:     maxUses,
		UseCount:    0,
		CreatedAt:   time.Now(),
	}

	if expiresIn != nil {
		expires := time.Now().Add(*expiresIn)
		invite.ExpiresAt = &expires
	}

	_, err = s.db.Exec(ctx,
		`INSERT INTO community_invites (id, community_id, code, created_by, max_uses, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		invite.ID, invite.CommunityID, invite.Code, invite.CreatedBy, invite.MaxUses, invite.ExpiresAt, invite.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	s.LogAudit(ctx, &communityID, userID, models.AuditActionInviteCreate, "invite", &invite.ID, nil)

	return invite, nil
}

func (s *Service) GetInvites(ctx context.Context, communityID, userID uuid.UUID) ([]*models.CommunityInvite, error) {
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionCreateInvites); err != nil {
		return nil, err
	}

	canManageAll := false
	if s.requirePermission(ctx, communityID, userID, models.PermissionManageCommunity) == nil ||
		s.requirePermission(ctx, communityID, userID, models.PermissionAdministrator) == nil {
		canManageAll = true
	}

	var rows pgx.Rows
	var err error
	if canManageAll {
		rows, err = s.db.Query(ctx,
			`SELECT id, community_id, code, created_by, max_uses, use_count, expires_at, created_at
			FROM community_invites WHERE community_id = $1
			ORDER BY created_at DESC`,
			communityID,
		)
	} else {
		rows, err = s.db.Query(ctx,
			`SELECT id, community_id, code, created_by, max_uses, use_count, expires_at, created_at
			FROM community_invites WHERE community_id = $1 AND created_by = $2
			ORDER BY created_at DESC`,
			communityID, userID,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	invites := make([]*models.CommunityInvite, 0)
	for rows.Next() {
		i := &models.CommunityInvite{}
		err := rows.Scan(&i.ID, &i.CommunityID, &i.Code, &i.CreatedBy, &i.MaxUses, &i.UseCount, &i.ExpiresAt, &i.CreatedAt)
		if err != nil {
			return nil, err
		}
		invites = append(invites, i)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate invite rows")
		return nil, err
	}

	return invites, nil
}

func (s *Service) DeleteInvite(ctx context.Context, communityID, inviteID, userID uuid.UUID) error {
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionCreateInvites); err != nil {
		return err
	}

	canManageAll := false
	if s.requirePermission(ctx, communityID, userID, models.PermissionManageCommunity) == nil ||
		s.requirePermission(ctx, communityID, userID, models.PermissionAdministrator) == nil {
		canManageAll = true
	}

	var err error
	if canManageAll {
		_, err = s.db.Exec(ctx,
			`DELETE FROM community_invites WHERE id = $1 AND community_id = $2`,
			inviteID, communityID,
		)
	} else {
		_, err = s.db.Exec(ctx,
			`DELETE FROM community_invites WHERE id = $1 AND community_id = $2 AND created_by = $3`,
			inviteID, communityID, userID,
		)
	}

	if err == nil {
		s.LogAudit(ctx, &communityID, userID, models.AuditActionInviteDelete, "invite", &inviteID, nil)
	}
	return err
}

// Roles

func (s *Service) GetRoles(ctx context.Context, communityID uuid.UUID) ([]*models.Role, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, community_id, name, color, position, permissions, is_default, created_at, updated_at
		FROM roles WHERE community_id = $1
		ORDER BY position DESC`,
		communityID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roles []*models.Role
	for rows.Next() {
		r := &models.Role{}
		err := rows.Scan(&r.ID, &r.CommunityID, &r.Name, &r.Color, &r.Position, &r.Permissions, &r.IsDefault, &r.CreatedAt, &r.UpdatedAt)
		if err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate role rows")
		return nil, err
	}

	return roles, nil
}

type CreateRoleRequest struct {
	Name        string  `json:"name" validate:"required,min=1,max=64"`
	Color       *string `json:"color" validate:"omitempty,hexcolor"`
	Permissions int64   `json:"permissions"`
}

type UpdateRoleRequest struct {
	Name        *string `json:"name" validate:"omitempty,min=1,max=64"`
	Color       *string `json:"color" validate:"omitempty,hexcolor"`
	Permissions *int64  `json:"permissions"`
}

func (s *Service) CreateRole(ctx context.Context, communityID, userID uuid.UUID, req *CreateRoleRequest) (*models.Role, error) {
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionManageRoles); err != nil {
		return nil, err
	}

	// Get max position
	var maxPos int
	if err := s.db.QueryRow(ctx,
		`SELECT COALESCE(MAX(position), 0) FROM roles WHERE community_id = $1`,
		communityID,
	).Scan(&maxPos); err != nil {
		log.Warn().Err(err).Msg("Failed to get max role position")
	}

	role := &models.Role{
		ID:          uuid.New(),
		CommunityID: communityID,
		Name:        req.Name,
		Color:       req.Color,
		Position:    maxPos + 1,
		Permissions: req.Permissions,
		IsDefault:   false,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	_, err := s.db.Exec(ctx,
		`INSERT INTO roles (id, community_id, name, color, position, permissions, is_default, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		role.ID, role.CommunityID, role.Name, role.Color, role.Position, role.Permissions, role.IsDefault, role.CreatedAt, role.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	details, _ := json.Marshal(map[string]string{"name": role.Name})
	s.LogAudit(ctx, &communityID, userID, models.AuditActionRoleCreate, "role", &role.ID, details)

	return role, nil
}

func (s *Service) DeleteRole(ctx context.Context, communityID, roleID, userID uuid.UUID) error {
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionManageRoles); err != nil {
		return err
	}

	// Cannot delete default role
	var isDefault bool
	err := s.db.QueryRow(ctx,
		`SELECT is_default FROM roles WHERE id = $1 AND community_id = $2`,
		roleID, communityID,
	).Scan(&isDefault)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRoleNotFound
		}
		return err
	}
	if isDefault {
		return errors.New("cannot delete the default role")
	}

	_, err = s.db.Exec(ctx, `DELETE FROM roles WHERE id = $1 AND community_id = $2`, roleID, communityID)
	if err == nil {
		s.LogAudit(ctx, &communityID, userID, models.AuditActionRoleDelete, "role", &roleID, nil)
	}
	return err
}

func (s *Service) UpdateRole(ctx context.Context, communityID, roleID, userID uuid.UUID, req *UpdateRoleRequest) (*models.Role, error) {
	if err := s.requirePermission(ctx, communityID, userID, models.PermissionManageRoles); err != nil {
		return nil, err
	}

	role := &models.Role{}
	if err := s.db.QueryRow(ctx,
		`SELECT id, community_id, name, color, position, permissions, is_default, created_at, updated_at
		FROM roles WHERE id = $1 AND community_id = $2`,
		roleID, communityID,
	).Scan(
		&role.ID, &role.CommunityID, &role.Name, &role.Color, &role.Position,
		&role.Permissions, &role.IsDefault, &role.CreatedAt, &role.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRoleNotFound
		}
		return nil, err
	}

	_, err := s.db.Exec(ctx,
		`UPDATE roles SET
			name = COALESCE($3, name),
			color = COALESCE($4, color),
			permissions = COALESCE($5, permissions),
			updated_at = NOW()
		WHERE id = $1 AND community_id = $2`,
		roleID, communityID, req.Name, req.Color, req.Permissions,
	)
	if err != nil {
		return nil, err
	}

	changes := map[string]interface{}{}
	if req.Name != nil {
		changes["name"] = *req.Name
	}
	if req.Permissions != nil {
		changes["permissions"] = *req.Permissions
	}
	if len(changes) > 0 {
		details, _ := json.Marshal(changes)
		s.LogAudit(ctx, &communityID, userID, models.AuditActionRoleUpdate, "role", &roleID, details)
	}

	return s.GetRole(ctx, communityID, roleID)
}

func (s *Service) GetRole(ctx context.Context, communityID, roleID uuid.UUID) (*models.Role, error) {
	role := &models.Role{}
	err := s.db.QueryRow(ctx,
		`SELECT id, community_id, name, color, position, permissions, is_default, created_at, updated_at
		FROM roles WHERE id = $1 AND community_id = $2`,
		roleID, communityID,
	).Scan(
		&role.ID, &role.CommunityID, &role.Name, &role.Color, &role.Position,
		&role.Permissions, &role.IsDefault, &role.CreatedAt, &role.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRoleNotFound
		}
		return nil, err
	}

	return role, nil
}

func (s *Service) GetDefaultRole(ctx context.Context, communityID uuid.UUID) (*models.Role, error) {
	role := &models.Role{}
	err := s.db.QueryRow(ctx,
		`SELECT id, community_id, name, color, position, permissions, is_default, created_at, updated_at
		FROM roles WHERE community_id = $1 AND is_default = TRUE`,
		communityID,
	).Scan(
		&role.ID, &role.CommunityID, &role.Name, &role.Color, &role.Position,
		&role.Permissions, &role.IsDefault, &role.CreatedAt, &role.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRoleNotFound
		}
		return nil, err
	}

	return role, nil
}

func (s *Service) GetMemberRoles(ctx context.Context, communityID, userID uuid.UUID) ([]*models.Role, error) {
	member, err := s.GetMember(ctx, communityID, userID)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT r.id, r.community_id, r.name, r.color, r.position, r.permissions, r.is_default, r.created_at, r.updated_at
		FROM member_roles mr
		JOIN roles r ON r.id = mr.role_id
		WHERE mr.member_id = $1
		ORDER BY r.position DESC`,
		member.ID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roles []*models.Role
	for rows.Next() {
		r := &models.Role{}
		err := rows.Scan(
			&r.ID, &r.CommunityID, &r.Name, &r.Color, &r.Position,
			&r.Permissions, &r.IsDefault, &r.CreatedAt, &r.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate member roles rows")
		return nil, err
	}

	if len(roles) == 0 {
		defaultRole, err := s.GetDefaultRole(ctx, communityID)
		if err == nil && defaultRole != nil {
			roles = append(roles, defaultRole)
		}
	}

	return roles, nil
}

func (s *Service) GetMemberRoleIDs(ctx context.Context, communityID, userID uuid.UUID) ([]uuid.UUID, error) {
	member, err := s.GetMember(ctx, communityID, userID)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT role_id FROM member_roles WHERE member_id = $1`,
		member.ID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roleIDs []uuid.UUID
	for rows.Next() {
		var roleID uuid.UUID
		if err := rows.Scan(&roleID); err != nil {
			return nil, err
		}
		roleIDs = append(roleIDs, roleID)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate member role ID rows")
		return nil, err
	}

	return roleIDs, nil
}

func (s *Service) SetMemberRoles(ctx context.Context, communityID, actorID, targetID uuid.UUID, roleIDs []uuid.UUID) error {
	if err := s.requirePermission(ctx, communityID, actorID, models.PermissionManageRoles); err != nil {
		return err
	}

	community, err := s.GetCommunity(ctx, communityID)
	if err != nil {
		return err
	}
	if community.OwnerID == targetID && community.OwnerID != actorID {
		return ErrNotOwner
	}

	member, err := s.GetMember(ctx, communityID, targetID)
	if err != nil {
		return err
	}

	uniqueRoleIDs := make(map[uuid.UUID]struct{}, len(roleIDs))
	for _, roleID := range roleIDs {
		uniqueRoleIDs[roleID] = struct{}{}
	}

	filteredIDs := make([]uuid.UUID, 0, len(uniqueRoleIDs))
	if len(uniqueRoleIDs) > 0 {
		roleIDList := make([]uuid.UUID, 0, len(uniqueRoleIDs))
		for roleID := range uniqueRoleIDs {
			roleIDList = append(roleIDList, roleID)
		}

		rows, err := s.db.Query(ctx,
			`SELECT id, is_default FROM roles WHERE community_id = $1 AND id = ANY($2)`,
			communityID, roleIDList,
		)
		if err != nil {
			return err
		}
		defer rows.Close()

		foundIDs := make(map[uuid.UUID]bool, len(roleIDList))
		filteredIDs = filteredIDs[:0]
		for rows.Next() {
			var roleID uuid.UUID
			var isDefault bool
			if err := rows.Scan(&roleID, &isDefault); err != nil {
				return err
			}
			foundIDs[roleID] = true
			if !isDefault {
				filteredIDs = append(filteredIDs, roleID)
			}
		}

		if err := rows.Err(); err != nil {
			log.Warn().Err(err).Msg("Failed to iterate set member roles rows")
			return err
		}

		if len(foundIDs) != len(roleIDList) {
			return ErrRoleNotFound
		}
	}

	return database.WithTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM member_roles WHERE member_id = $1`,
			member.ID,
		)
		if err != nil {
			return err
		}

		for _, roleID := range filteredIDs {
			_, err := tx.Exec(ctx,
				`INSERT INTO member_roles (member_id, role_id) VALUES ($1, $2)`,
				member.ID, roleID,
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// Permission helpers

func (s *Service) RequirePermission(ctx context.Context, communityID, userID uuid.UUID, permission int64) error {
	return s.requirePermission(ctx, communityID, userID, permission)
}

func (s *Service) requirePermission(ctx context.Context, communityID, userID uuid.UUID, permission int64) error {
	userPermissions, err := s.GetMemberPermissions(ctx, communityID, userID)
	if err != nil {
		return err
	}

	if !models.HasPermission(userPermissions, permission) {
		return ErrInsufficientPerms
	}

	return nil
}

func (s *Service) GetMemberPermissions(ctx context.Context, communityID, userID uuid.UUID) (int64, error) {
	member, err := s.GetMember(ctx, communityID, userID)
	if err != nil {
		return 0, err
	}

	community, err := s.GetCommunity(ctx, communityID)
	if err != nil {
		return 0, err
	}

	if community.OwnerID == userID {
		return models.PermissionAdministrator, nil
	}

	var userPermissions int64
	var roleCount int
	err = s.db.QueryRow(ctx,
		`SELECT COALESCE(BIT_OR(r.permissions), 0), COUNT(r.id)
		FROM member_roles mr
		JOIN roles r ON r.id = mr.role_id
		WHERE mr.member_id = $1`,
		member.ID,
	).Scan(&userPermissions, &roleCount)
	if err != nil {
		return 0, err
	}

	if roleCount == 0 {
		err = s.db.QueryRow(ctx,
			`SELECT permissions FROM roles WHERE community_id = $1 AND is_default = TRUE`,
			communityID,
		).Scan(&userPermissions)
		if err != nil {
			return 0, ErrInsufficientPerms
		}
	}

	return userPermissions, nil
}

func (s *Service) GetMemberPermissionsForMember(ctx context.Context, communityID uuid.UUID, member *models.CommunityMember) (int64, error) {
	community, err := s.GetCommunity(ctx, communityID)
	if err != nil {
		return 0, err
	}

	if community.OwnerID == member.UserID {
		return models.PermissionAdministrator, nil
	}

	var userPermissions int64
	var roleCount int
	err = s.db.QueryRow(ctx,
		`SELECT COALESCE(BIT_OR(r.permissions), 0), COUNT(r.id)
		FROM member_roles mr
		JOIN roles r ON r.id = mr.role_id
		WHERE mr.member_id = $1`,
		member.ID,
	).Scan(&userPermissions, &roleCount)
	if err != nil {
		return 0, err
	}

	if roleCount == 0 {
		err = s.db.QueryRow(ctx,
			`SELECT permissions FROM roles WHERE community_id = $1 AND is_default = TRUE`,
			communityID,
		).Scan(&userPermissions)
		if err != nil {
			return 0, ErrInsufficientPerms
		}
	}

	return userPermissions, nil
}

func (s *Service) ReorderCommunities(ctx context.Context, userID uuid.UUID, communityIDs []uuid.UUID) error {
	layout, err := json.Marshal(map[string][]uuid.UUID{"communityOrder": communityIDs})
	if err != nil {
		return err
	}

	_, err = s.db.Exec(ctx,
		`UPDATE users SET community_layout = $1 WHERE id = $2`,
		layout, userID,
	)
	return err
}

func (s *Service) IsMember(ctx context.Context, communityID, userID uuid.UUID) bool {
	_, err := s.GetMember(ctx, communityID, userID)
	return err == nil
}
