package channel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
	"github.com/zentra/server/internal/services/channeltype"
	"github.com/zentra/server/internal/services/community"
	"github.com/zentra/server/internal/services/notification"
	"github.com/zentra/server/pkg/database"
)

var (
	ErrChannelNotFound    = errors.New("channel not found")
	ErrCategoryNotFound   = errors.New("category not found")
	ErrInsufficientPerms  = errors.New("insufficient permissions")
	ErrInvalidChannelType = errors.New("invalid channel type")
)

type Service struct {
	db                 *pgxpool.Pool
	communityService   *community.Service
	typeRegistry       *channeltype.Registry
	notificationService *notification.Service
}

func NewService(db *pgxpool.Pool, communityService *community.Service, typeRegistry *channeltype.Registry) *Service {
	return &Service{
		db:               db,
		communityService: communityService,
		typeRegistry:     typeRegistry,
	}
}

// SetNotificationService wires the notification service after construction
// (both services depend on the wsHub which is created after them).
func (s *Service) SetNotificationService(ns *notification.Service) {
	s.notificationService = ns
}

type CreateChannelRequest struct {
	Name            string          `json:"name" validate:"required,channelname"`
	Topic           *string         `json:"topic" validate:"omitempty,max=1024"`
	Type            string          `json:"type" validate:"required,min=1,max=64"`
	CategoryID      *uuid.UUID      `json:"categoryId"`
	IsNSFW          bool            `json:"isNsfw"`
	SlowmodeSeconds int             `json:"slowmodeSeconds" validate:"min=0,max=21600"`
	Metadata        json.RawMessage `json:"metadata"`
}

func (s *Service) CreateChannel(ctx context.Context, communityID, userID uuid.UUID, req *CreateChannelRequest) (*models.Channel, error) {
	// Check permissions
	if err := s.requireChannelPermission(ctx, communityID, userID, models.PermissionManageChannels); err != nil {
		return nil, err
	}

	// Validate that the requested type actually exists in the registry
	typeDef, err := s.typeRegistry.Get(req.Type)
	if err != nil {
		return nil, ErrInvalidChannelType
	}

	// Use the type's default metadata if none was provided
	metadata := req.Metadata
	if metadata == nil || string(metadata) == "" || string(metadata) == "null" {
		metadata = typeDef.DefaultMetadata
	}
	if metadata == nil {
		metadata = json.RawMessage("{}")
	}

	// Get max position
	var maxPos int
	if err := s.db.QueryRow(ctx,
		`SELECT COALESCE(MAX(position), -1) FROM channels WHERE community_id = $1`,
		communityID,
	).Scan(&maxPos); err != nil {
		log.Warn().Err(err).Msg("Failed to get max channel position")
	}

	channel := &models.Channel{
		ID:              uuid.New(),
		CommunityID:     communityID,
		CategoryID:      req.CategoryID,
		Name:            req.Name,
		Topic:           req.Topic,
		Type:            models.ChannelType(req.Type),
		Position:        maxPos + 1,
		IsNSFW:          req.IsNSFW,
		SlowmodeSeconds: req.SlowmodeSeconds,
		Metadata:        metadata,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	_, err = s.db.Exec(ctx,
		`INSERT INTO channels (id, community_id, category_id, name, topic, type, position, is_nsfw, slowmode_seconds, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		channel.ID, channel.CommunityID, channel.CategoryID, channel.Name, channel.Topic,
		channel.Type, channel.Position, channel.IsNSFW, channel.SlowmodeSeconds, channel.Metadata,
		channel.CreatedAt, channel.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	details, _ := json.Marshal(map[string]string{"name": channel.Name, "type": string(channel.Type)})
	s.communityService.LogAudit(ctx, &communityID, userID, models.AuditActionChannelCreate, "channel", &channel.ID, details)

	return channel, nil
}

func (s *Service) GetChannel(ctx context.Context, id uuid.UUID) (*models.Channel, error) {
	channel := &models.Channel{}
	err := s.db.QueryRow(ctx,
		`SELECT id, community_id, category_id, name, topic, type, position, is_nsfw, slowmode_seconds, metadata, created_at, updated_at
		FROM channels WHERE id = $1`,
		id,
	).Scan(
		&channel.ID, &channel.CommunityID, &channel.CategoryID, &channel.Name, &channel.Topic,
		&channel.Type, &channel.Position, &channel.IsNSFW, &channel.SlowmodeSeconds, &channel.Metadata,
		&channel.CreatedAt, &channel.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrChannelNotFound
		}
		return nil, err
	}
	return channel, nil
}

func (s *Service) GetCommunityChannels(ctx context.Context, communityID, userID uuid.UUID) ([]*models.ChannelWithCategory, error) {
	rows, err := s.db.Query(ctx,
		`SELECT c.id, c.community_id, c.category_id, c.name, c.topic, c.type, c.position, 
		c.is_nsfw, c.slowmode_seconds, c.metadata, c.created_at, c.updated_at, cat.name as category_name
		FROM channels c
		LEFT JOIN channel_categories cat ON cat.id = c.category_id
		WHERE c.community_id = $1
		ORDER BY cat.position NULLS FIRST, c.position`,
		communityID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []*models.ChannelWithCategory
	for rows.Next() {
		c := &models.ChannelWithCategory{}
		err := rows.Scan(
			&c.ID, &c.CommunityID, &c.CategoryID, &c.Name, &c.Topic, &c.Type,
			&c.Position, &c.IsNSFW, &c.SlowmodeSeconds, &c.Metadata, &c.CreatedAt, &c.UpdatedAt, &c.CategoryName,
		)
		if err != nil {
			return nil, err
		}
		channels = append(channels, c)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate channel rows")
		return nil, err
	}

	if len(channels) == 0 {
		return channels, nil
	}

	// Batch permission resolution for all channels
	basePermissions, err := s.communityService.GetMemberPermissions(ctx, communityID, userID)
	if err != nil {
		return nil, err
	}

	if basePermissions&models.PermissionAdministrator != 0 {
		return channels, nil
	}

	member, err := s.communityService.GetMember(ctx, communityID, userID)
	if err != nil {
		return nil, err
	}

	roleIDs, err := s.communityService.GetMemberRoleIDs(ctx, communityID, userID)
	if err != nil {
		return nil, err
	}
	if roleIDs == nil {
		roleIDs = []uuid.UUID{}
	}

	defaultRole, err := s.communityService.GetDefaultRole(ctx, communityID)
	if err == nil && defaultRole != nil {
		roleIDs = append(roleIDs, defaultRole.ID)
	}

	permRows, err := s.db.Query(ctx,
		`SELECT cp.channel_id, cp.target_type, cp.allow_permissions, cp.deny_permissions
		FROM channel_permissions cp
		WHERE cp.channel_id = ANY(SELECT id FROM channels WHERE community_id = $1)
		AND (
			(cp.target_type = 'role' AND cp.target_id = ANY($2))
			OR (cp.target_type = 'member' AND cp.target_id = $3)
		)`,
		communityID, roleIDs, member.ID,
	)
	if err != nil {
		return nil, err
	}
	defer permRows.Close()

	type channelOverride struct {
		roleAllow   int64
		roleDeny    int64
		memberAllow int64
		memberDeny  int64
	}
	overrides := make(map[uuid.UUID]*channelOverride)
	for permRows.Next() {
		var channelID uuid.UUID
		var targetType string
		var allowPerms, denyPerms int64
		if err := permRows.Scan(&channelID, &targetType, &allowPerms, &denyPerms); err != nil {
			return nil, err
		}

		co, ok := overrides[channelID]
		if !ok {
			co = &channelOverride{}
			overrides[channelID] = co
		}

		if targetType == "member" {
			co.memberAllow |= allowPerms
			co.memberDeny |= denyPerms
		} else {
			co.roleAllow |= allowPerms
			co.roleDeny |= denyPerms
		}
	}
	if err := permRows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate channel permission rows")
		return nil, err
	}

	accessible := channels[:0]
	for _, ch := range channels {
		perms := basePermissions
		if co, ok := overrides[ch.ID]; ok {
			perms &= ^co.roleDeny
			perms |= co.roleAllow
			perms &= ^co.memberDeny
			perms |= co.memberAllow
		}
		if models.HasPermission(perms, models.PermissionViewChannels) {
			accessible = append(accessible, ch)
		}
	}

	return accessible, nil
}

type UpdateChannelRequest struct {
	Name            *string    `json:"name" validate:"omitempty,channelname"`
	Topic           *string    `json:"topic" validate:"omitempty,max=1024"`
	CategoryID      *uuid.UUID `json:"categoryId"`
	IsNSFW          *bool      `json:"isNsfw"`
	SlowmodeSeconds *int       `json:"slowmodeSeconds" validate:"omitempty,min=0,max=21600"`
}

func (s *Service) UpdateChannel(ctx context.Context, channelID, userID uuid.UUID, req *UpdateChannelRequest) (*models.Channel, error) {
	channel, err := s.GetChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}

	if err := s.requireChannelPermission(ctx, channel.CommunityID, userID, models.PermissionManageChannels); err != nil {
		return nil, err
	}

	_, err = s.db.Exec(ctx,
		`UPDATE channels SET 
			name = COALESCE($2, name),
			topic = COALESCE($3, topic),
			category_id = COALESCE($4, category_id),
			is_nsfw = COALESCE($5, is_nsfw),
			slowmode_seconds = COALESCE($6, slowmode_seconds),
			updated_at = NOW()
		WHERE id = $1`,
		channelID, req.Name, req.Topic, req.CategoryID, req.IsNSFW, req.SlowmodeSeconds,
	)
	if err != nil {
		return nil, err
	}

	changes := map[string]interface{}{}
	if req.Name != nil {
		changes["name"] = *req.Name
	}
	if req.Topic != nil {
		changes["topic"] = *req.Topic
	}
	if len(changes) > 0 {
		details, _ := json.Marshal(changes)
		s.communityService.LogAudit(ctx, &channel.CommunityID, userID, models.AuditActionChannelUpdate, "channel", &channelID, details)
	}

	return s.GetChannel(ctx, channelID)
}

func (s *Service) DeleteChannel(ctx context.Context, channelID, userID uuid.UUID) error {
	channel, err := s.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}

	if err := s.requireChannelPermission(ctx, channel.CommunityID, userID, models.PermissionManageChannels); err != nil {
		return err
	}

	details, _ := json.Marshal(map[string]string{"name": channel.Name})

	_, err = s.db.Exec(ctx, `DELETE FROM channels WHERE id = $1`, channelID)
	if err == nil {
		s.communityService.LogAudit(ctx, &channel.CommunityID, userID, models.AuditActionChannelDelete, "channel", &channelID, details)
	}
	return err
}

func (s *Service) ReorderChannels(ctx context.Context, communityID, userID uuid.UUID, channelIDs []uuid.UUID) error {
	if err := s.requireChannelPermission(ctx, communityID, userID, models.PermissionManageChannels); err != nil {
		return err
	}

	return database.WithTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for i, channelID := range channelIDs {
			_, err := tx.Exec(ctx,
				`UPDATE channels SET position = $2 WHERE id = $1 AND community_id = $3`,
				channelID, i, communityID,
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Categories

type CreateCategoryRequest struct {
	Name string `json:"name" validate:"required,min=1,max=64"`
}

func (s *Service) CreateCategory(ctx context.Context, communityID, userID uuid.UUID, req *CreateCategoryRequest) (*models.ChannelCategory, error) {
	if err := s.requireChannelPermission(ctx, communityID, userID, models.PermissionManageChannels); err != nil {
		return nil, err
	}

	var maxPos int
	if err := s.db.QueryRow(ctx,
		`SELECT COALESCE(MAX(position), -1) FROM channel_categories WHERE community_id = $1`,
		communityID,
	).Scan(&maxPos); err != nil {
		log.Warn().Err(err).Msg("Failed to get max category position")
	}

	category := &models.ChannelCategory{
		ID:          uuid.New(),
		CommunityID: communityID,
		Name:        req.Name,
		Position:    maxPos + 1,
		CreatedAt:   time.Now(),
	}

	_, err := s.db.Exec(ctx,
		`INSERT INTO channel_categories (id, community_id, name, position, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		category.ID, category.CommunityID, category.Name, category.Position, category.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	return category, nil
}

func (s *Service) GetCategories(ctx context.Context, communityID uuid.UUID) ([]*models.ChannelCategory, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, community_id, name, position, created_at
		FROM channel_categories WHERE community_id = $1
		ORDER BY position`,
		communityID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var categories []*models.ChannelCategory
	for rows.Next() {
		c := &models.ChannelCategory{}
		err := rows.Scan(&c.ID, &c.CommunityID, &c.Name, &c.Position, &c.CreatedAt)
		if err != nil {
			return nil, err
		}
		categories = append(categories, c)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate category rows")
		return nil, err
	}

	return categories, nil
}

func (s *Service) UpdateCategory(ctx context.Context, categoryID, userID uuid.UUID, name string) (*models.ChannelCategory, error) {
	var communityID uuid.UUID
	err := s.db.QueryRow(ctx,
		`SELECT community_id FROM channel_categories WHERE id = $1`,
		categoryID,
	).Scan(&communityID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCategoryNotFound
		}
		return nil, err
	}

	if err := s.requireChannelPermission(ctx, communityID, userID, models.PermissionManageChannels); err != nil {
		return nil, err
	}

	_, err = s.db.Exec(ctx,
		`UPDATE channel_categories SET name = $2 WHERE id = $1`,
		categoryID, name,
	)
	if err != nil {
		return nil, err
	}

	category := &models.ChannelCategory{}
	err = s.db.QueryRow(ctx,
		`SELECT id, community_id, name, position, created_at FROM channel_categories WHERE id = $1`,
		categoryID,
	).Scan(&category.ID, &category.CommunityID, &category.Name, &category.Position, &category.CreatedAt)
	if err != nil {
		return nil, err
	}

	return category, nil
}

func (s *Service) DeleteCategory(ctx context.Context, categoryID, userID uuid.UUID) error {
	var communityID uuid.UUID
	err := s.db.QueryRow(ctx,
		`SELECT community_id FROM channel_categories WHERE id = $1`,
		categoryID,
	).Scan(&communityID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCategoryNotFound
		}
		return err
	}

	if err := s.requireChannelPermission(ctx, communityID, userID, models.PermissionManageChannels); err != nil {
		return err
	}

	_, err = s.db.Exec(ctx,
		`UPDATE channels SET category_id = NULL WHERE category_id = $1`,
		categoryID,
	)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(ctx, `DELETE FROM channel_categories WHERE id = $1`, categoryID)
	return err
}

func (s *Service) ReorderCategories(ctx context.Context, communityID, userID uuid.UUID, categoryIDs []uuid.UUID) error {
	if err := s.requireChannelPermission(ctx, communityID, userID, models.PermissionManageChannels); err != nil {
		return err
	}

	return database.WithTransaction(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for i, categoryID := range categoryIDs {
			_, err := tx.Exec(ctx,
				`UPDATE channel_categories SET position = $2 WHERE id = $1 AND community_id = $3`,
				categoryID, i, communityID,
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Channel Permissions
func (s *Service) GetChannelPermissions(ctx context.Context, channelID, userID uuid.UUID) ([]*models.ChannelPermission, error) {
	channel, err := s.GetChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}

	if err := s.requireChannelPermission(ctx, channel.CommunityID, userID, models.PermissionManageChannels); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT id, channel_id, target_type, target_id, allow_permissions, deny_permissions
		FROM channel_permissions WHERE channel_id = $1`,
		channelID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	perms := make([]*models.ChannelPermission, 0)
	for rows.Next() {
		p := &models.ChannelPermission{}
		err := rows.Scan(&p.ID, &p.ChannelID, &p.TargetType, &p.TargetID, &p.AllowPermissions, &p.DenyPermissions)
		if err != nil {
			return nil, err
		}
		perms = append(perms, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return perms, nil
}

type SetChannelPermissionRequest struct {
	TargetType       string    `json:"targetType" validate:"required,oneof=role member"`
	TargetID         uuid.UUID `json:"targetId" validate:"required"`
	AllowPermissions int64     `json:"allowPermissions"`
	DenyPermissions  int64     `json:"denyPermissions"`
}

func (s *Service) SetChannelPermission(ctx context.Context, channelID, userID uuid.UUID, req *SetChannelPermissionRequest) error {
	channel, err := s.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}

	if err := s.requireChannelPermission(ctx, channel.CommunityID, userID, models.PermissionManageChannels); err != nil {
		return err
	}

	_, err = s.db.Exec(ctx,
		`INSERT INTO channel_permissions (id, channel_id, target_type, target_id, allow_permissions, deny_permissions)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (channel_id, target_type, target_id) 
		DO UPDATE SET allow_permissions = $5, deny_permissions = $6`,
		uuid.New(), channelID, req.TargetType, req.TargetID, req.AllowPermissions, req.DenyPermissions,
	)
	return err
}

func (s *Service) DeleteChannelPermission(ctx context.Context, channelID, userID uuid.UUID, targetType string, targetID uuid.UUID) error {
	channel, err := s.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}

	if err := s.requireChannelPermission(ctx, channel.CommunityID, userID, models.PermissionManageChannels); err != nil {
		return err
	}

	_, err = s.db.Exec(ctx,
		`DELETE FROM channel_permissions WHERE channel_id = $1 AND target_type = $2 AND target_id = $3`,
		channelID, targetType, targetID,
	)
	return err
}

// Channel Read State
func (s *Service) MarkRead(ctx context.Context, channelID, userID uuid.UUID) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO channel_read_states (user_id, channel_id, last_read_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id, channel_id)
		DO UPDATE SET last_read_at = NOW()`,
		userID, channelID,
	)
	if err != nil {
		return err
	}

	if s.notificationService != nil {
		if err := s.notificationService.MarkChannelRead(ctx, channelID, userID); err != nil {
			log.Warn().Err(err).Msg("Failed to mark channel notifications as read")
		}
	}

	return nil
}

func (s *Service) GetUnreadCount(ctx context.Context, channelID, userID uuid.UUID) (int, error) {
	var count int
	var lastRead *time.Time

	err := s.db.QueryRow(ctx,
		`SELECT last_read_at FROM channel_read_states WHERE user_id = $1 AND channel_id = $2`,
		userID, channelID,
	).Scan(&lastRead)

	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Warn().Err(err).Msg("Failed to get channel read state")
		return 0, nil
	}

	var since time.Time
	if lastRead != nil {
		since = *lastRead
	} else {
		since = time.Unix(0, 0)
	}

	err = s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM messages
		WHERE channel_id = $1 AND deleted_at IS NULL
		AND created_at > $2 AND author_id <> $3`,
		channelID, since, userID,
	).Scan(&count)

	if err != nil {
		log.Warn().Err(err).Msg("Failed to count unread messages")
		return 0, nil
	}

	return count, nil
}

type CommunityUnreadResponse struct {
	Unread   map[string]int `json:"unread"`
	Mentions map[string]int `json:"mentions"`
}

func (s *Service) GetCommunityUnreadCounts(ctx context.Context, communityID, userID uuid.UUID) (*CommunityUnreadResponse, error) {
	rows, err := s.db.Query(ctx,
		`SELECT c.id,
			COALESCE((
				SELECT COUNT(*) FROM messages m
				WHERE m.channel_id = c.id AND m.deleted_at IS NULL
				AND m.created_at > COALESCE(crs.last_read_at, '1970-01-01'::timestamptz)
				AND m.author_id <> $2
			), 0)::int,
			COALESCE(mn.mention_count, 0)::int
		FROM channels c
		LEFT JOIN channel_read_states crs ON crs.channel_id = c.id AND crs.user_id = $2
		LEFT JOIN (
			SELECT n.channel_id, COUNT(*)::int AS mention_count
			FROM notifications n
			WHERE n.user_id = $2 AND n.is_read = FALSE
			AND n.type IN ('mention_user','mention_role','mention_everyone','mention_here','reply')
			AND n.channel_id IN (SELECT id FROM channels WHERE community_id = $1)
			GROUP BY n.channel_id
		) mn ON mn.channel_id = c.id
		WHERE c.community_id = $1
		ORDER BY c.position`,
		communityID, userID,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get community unread counts")
		return nil, err
	}
	defer rows.Close()

	unread := make(map[string]int)
	mentions := make(map[string]int)

	for rows.Next() {
		var channelID uuid.UUID
		var unreadCount int
		var mentionCount int
		if err := rows.Scan(&channelID, &unreadCount, &mentionCount); err != nil {
			log.Warn().Err(err).Msg("Failed to scan unread count row")
			continue
		}
		if unreadCount > 0 {
			unread[channelID.String()] = unreadCount
		}
		if mentionCount > 0 {
			mentions[channelID.String()] = mentionCount
		}
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate unread count rows")
		return nil, err
	}

	return &CommunityUnreadResponse{Unread: unread, Mentions: mentions}, nil
}

// Permission helpers
// I don't like this function, but it will do for now.
func (s *Service) requireChannelPermission(ctx context.Context, communityID, userID uuid.UUID, permission int64) error {
	if err := s.communityService.RequirePermission(ctx, communityID, userID, permission); err != nil {
		return ErrInsufficientPerms
	}

	return nil
}

func (s *Service) CanAccessChannel(ctx context.Context, channelID, userID uuid.UUID) bool {
	permissions, err := s.getChannelPermissions(ctx, channelID, userID)
	if err != nil {
		return false
	}

	return models.HasPermission(permissions, models.PermissionViewChannels)
}

func (s *Service) CanSendMessage(ctx context.Context, channelID, userID uuid.UUID) bool {
	permissions, err := s.getChannelPermissions(ctx, channelID, userID)
	if err != nil {
		return false
	}

	return models.HasPermission(permissions, models.PermissionSendMessages)
}

func (s *Service) CanManageMessages(ctx context.Context, channelID, userID uuid.UUID) bool {
	permissions, err := s.getChannelPermissions(ctx, channelID, userID)
	if err != nil {
		return false
	}

	return models.HasPermission(permissions, models.PermissionManageMessages)
}

func (s *Service) CanPinMessages(ctx context.Context, channelID, userID uuid.UUID) bool {
	permissions, err := s.getChannelPermissions(ctx, channelID, userID)
	if err != nil {
		return false
	}

	return models.HasPermission(permissions, models.PermissionPinMessages)
}

func (s *Service) CanManageWebhooks(ctx context.Context, channelID, userID uuid.UUID) bool {
	permissions, err := s.getChannelPermissions(ctx, channelID, userID)
	if err != nil {
		return false
	}

	return models.HasPermission(permissions, models.PermissionManageWebhooks)
}

func (s *Service) CanMentionEveryone(ctx context.Context, channelID, userID uuid.UUID) bool {
	permissions, err := s.getChannelPermissions(ctx, channelID, userID)
	if err != nil {
		return false
	}

	return models.HasPermission(permissions, models.PermissionMentionEveryone)
}

func (s *Service) getChannelPermissions(ctx context.Context, channelID, userID uuid.UUID) (int64, error) {
	channel, err := s.GetChannel(ctx, channelID)
	if err != nil {
		return 0, err
	}

	member, err := s.communityService.GetMember(ctx, channel.CommunityID, userID)
	if err != nil {
		return 0, err
	}

	basePermissions, err := s.communityService.GetMemberPermissionsForMember(ctx, channel.CommunityID, member)
	if err != nil {
		return 0, err
	}

	if basePermissions&models.PermissionAdministrator != 0 {
		return basePermissions, nil
	}

	roleIDs, err := s.communityService.GetMemberRoleIDs(ctx, channel.CommunityID, userID)
	if err != nil {
		return 0, err
	}
	if roleIDs == nil {
		roleIDs = []uuid.UUID{}
	}

	defaultRole, err := s.communityService.GetDefaultRole(ctx, channel.CommunityID)
	if err == nil && defaultRole != nil {
		roleIDs = append(roleIDs, defaultRole.ID)
	}

	rows, err := s.db.Query(ctx,
		`SELECT target_type, target_id, allow_permissions, deny_permissions
		FROM channel_permissions
		WHERE channel_id = $1
		AND (
			(target_type = 'role' AND target_id = ANY($2))
			OR (target_type = 'member' AND target_id = $3)
		)`,
		channelID, roleIDs, member.ID,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var roleAllow int64
	var roleDeny int64
	var memberAllow int64
	var memberDeny int64
	for rows.Next() {
		var targetType string
		var targetID uuid.UUID
		var allowPerms int64
		var denyPerms int64
		if err := rows.Scan(&targetType, &targetID, &allowPerms, &denyPerms); err != nil {
			return 0, err
		}

		if targetType == "member" {
			memberAllow |= allowPerms
			memberDeny |= denyPerms
			continue
		}

		roleAllow |= allowPerms
		roleDeny |= denyPerms
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Failed to iterate channel permission rows")
		return 0, err
	}

	permissions := basePermissions
	permissions &= ^roleDeny
	permissions |= roleAllow
	permissions &= ^memberDeny
	permissions |= memberAllow

	return permissions, nil
}
