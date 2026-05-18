package user

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"github.com/zentra/server/internal/models"
	"github.com/zentra/server/pkg/database"
)

var (
	ErrUserNotFound            = errors.New("user not found")
	ErrAlreadyBlocked          = errors.New("user already blocked")
	ErrNotBlocked              = errors.New("user not blocked")
	ErrAlreadyFriends          = errors.New("users are already friends")
	ErrFriendRequestExists     = errors.New("friend request already exists")
	ErrIncomingFriendRequest   = errors.New("incoming friend request exists")
	ErrFriendRequestNotFound   = errors.New("friend request not found")
	ErrNotFriends              = errors.New("users are not friends")
	ErrUsersBlocked            = errors.New("users are blocked")
	ErrCannotFriendYourself    = errors.New("cannot add yourself as a friend")
	ErrCannotAcceptOwnRequest  = errors.New("cannot accept your own friend request")
	ErrCannotRemoveSelfRequest = errors.New("cannot remove a friend request to yourself")
	ErrCannotRemoveSelfFriend  = errors.New("cannot remove yourself as a friend")
)

type Service struct {
	db    *pgxpool.Pool
	redis *redis.Client
}

func NewService(db *pgxpool.Pool, redis *redis.Client) *Service {
	return &Service{
		db:    db,
		redis: redis,
	}
}

func (s *Service) broadcast(ctx context.Context, userID uuid.UUID, eventType string, data interface{}) {
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
		ChannelID: "", // Global broadcast
		Event:     event,
	}

	jsonData, err := json.Marshal(broadcast)
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal user update broadcast")
		return
	}

	err = s.redis.Publish(ctx, "websocket:broadcast", jsonData).Err()
	if err != nil {
		log.Error().Err(err).Msg("Failed to publish user update to Redis")
	}
}

func (s *Service) GetUserByID(ctx context.Context, id uuid.UUID) (*models.User, error) {
	user := &models.User{}
	err := s.db.QueryRow(ctx,
		`SELECT id, username, email, display_name, avatar_url, bio, status, custom_status,
		email_verified, two_factor_enabled, created_at, updated_at, last_seen_at, is_admin
		FROM users WHERE id = $1 AND deleted_at IS NULL`,
		id,
	).Scan(
		&user.ID, &user.Username, &user.Email, &user.DisplayName, &user.AvatarURL,
		&user.Bio, &user.Status, &user.CustomStatus, &user.EmailVerified,
		&user.TwoFactorEnabled, &user.CreatedAt, &user.UpdatedAt, &user.LastSeenAt,
		&user.IsAdmin,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return user, nil
}

func (s *Service) GetPublicUser(ctx context.Context, id uuid.UUID) (*models.PublicUser, error) {
	user, err := s.GetUserByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return user.ToPublic(), nil
}

func (s *Service) GetUserByUsername(ctx context.Context, username string) (*models.PublicUser, error) {
	user := &models.User{}
	err := s.db.QueryRow(ctx,
		`SELECT id, username, display_name, avatar_url, bio, status, custom_status, created_at
		FROM users WHERE username = $1 AND deleted_at IS NULL`,
		username,
	).Scan(
		&user.ID, &user.Username, &user.DisplayName, &user.AvatarURL,
		&user.Bio, &user.Status, &user.CustomStatus, &user.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return user.ToPublic(), nil
}

type UpdateProfileRequest struct {
	DisplayName  *string `json:"displayName" validate:"omitempty,max=64"`
	Bio          *string `json:"bio" validate:"omitempty,max=500"`
	CustomStatus *string `json:"customStatus" validate:"omitempty,max=128"`
}

func (s *Service) UpdateProfile(ctx context.Context, userID uuid.UUID, req *UpdateProfileRequest) (*models.User, error) {
	_, err := s.db.Exec(ctx,
		`UPDATE users SET 
			display_name = COALESCE($2, display_name),
			bio = COALESCE($3, bio),
			custom_status = COALESCE($4, custom_status),
			updated_at = NOW()
		WHERE id = $1`,
		userID, req.DisplayName, req.Bio, req.CustomStatus,
	)
	if err != nil {
		return nil, err
	}

	user, err := s.GetUserByID(ctx, userID)
	if err == nil {
		s.broadcast(ctx, userID, "USER_UPDATE", user)
	}

	return user, err
}

func (s *Service) UpdateAvatar(ctx context.Context, userID uuid.UUID, avatarURL string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE users SET avatar_url = $2, updated_at = NOW() WHERE id = $1`,
		userID, avatarURL,
	)
	if err == nil {
		if user, err := s.GetUserByID(ctx, userID); err == nil {
			s.broadcast(ctx, userID, "USER_UPDATE", user)
		}
	}
	return err
}

func (s *Service) RemoveAvatar(ctx context.Context, userID uuid.UUID) error {
	_, err := s.db.Exec(ctx,
		`UPDATE users SET avatar_url = NULL, updated_at = NOW() WHERE id = $1`,
		userID,
	)
	if err == nil {
		if user, err := s.GetUserByID(ctx, userID); err == nil {
			s.broadcast(ctx, userID, "USER_UPDATE", user)
		}
	}
	return err
}

func (s *Service) UpdateStatus(ctx context.Context, userID uuid.UUID, status models.UserStatus) error {
	_, err := s.db.Exec(ctx,
		`UPDATE users
		SET status = $2,
			updated_at = NOW(),
			last_seen_at = CASE WHEN $2 = 'offline' THEN NOW() ELSE last_seen_at END
		WHERE id = $1`,
		userID, status,
	)
	if err != nil {
		return err
	}

	if user, err := s.GetUserByID(ctx, userID); err == nil {
		s.broadcast(ctx, userID, "USER_UPDATE", user)
		// Also send explicit presence update
		s.broadcast(ctx, userID, "PRESENCE_UPDATE", map[string]interface{}{
			"userId": userID.String(),
			"status": string(status),
		})
	}

	// Also update Redis presence
	return database.SetUserPresence(ctx, userID.String(), string(status), 0)
}

// MarkAllUsersOffline clears stale online/away/busy/invisible states.
// Startup can call this so presence is rebuilt from live websocket connections.
func (s *Service) MarkAllUsersOffline(ctx context.Context) error {
	_, err := s.db.Exec(ctx,
		`UPDATE users
		SET status = 'offline',
			updated_at = NOW(),
			last_seen_at = CASE WHEN status <> 'offline' THEN NOW() ELSE last_seen_at END
		WHERE deleted_at IS NULL
		  AND status <> 'offline'`,
	)
	if err != nil {
		return err
	}

	if s.redis == nil {
		return nil
	}

	if err := clearRedisKeysByPattern(ctx, s.redis, "presence:user:*"); err != nil {
		log.Warn().Err(err).Msg("Failed to clear presence:user:* keys")
	}

	if err := clearRedisKeysByPattern(ctx, s.redis, "presence:????????-????-????-????-????????????"); err != nil {
		log.Warn().Err(err).Msg("Failed to clear legacy presence:* keys")
	}

	return nil
}

func clearRedisKeysByPattern(ctx context.Context, client *redis.Client, pattern string) error {
	var cursor uint64

	for {
		keys, nextCursor, err := client.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			return err
		}

		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	return nil
}

func (s *Service) SearchUsers(ctx context.Context, query string, limit, offset int) ([]*models.PublicUser, int64, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	var total int64
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM users 
		WHERE deleted_at IS NULL AND (username ILIKE $1 OR display_name ILIKE $1)`,
		"%"+query+"%",
	).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT id, username, display_name, avatar_url, bio, status, custom_status, created_at
		FROM users 
		WHERE deleted_at IS NULL AND (username ILIKE $1 OR display_name ILIKE $1)
		ORDER BY username
		LIMIT $2 OFFSET $3`,
		"%"+query+"%", limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var users []*models.PublicUser
	for rows.Next() {
		user := &models.PublicUser{}
		err := rows.Scan(
			&user.ID, &user.Username, &user.DisplayName, &user.AvatarURL,
			&user.Bio, &user.Status, &user.CustomStatus, &user.CreatedAt,
		)
		if err != nil {
			return nil, 0, err
		}
		users = append(users, user)
	}

	return users, total, nil
}

// User Settings

func (s *Service) GetSettings(ctx context.Context, userID uuid.UUID) (*models.UserSettings, error) {
	settings := &models.UserSettings{}
	err := s.db.QueryRow(ctx,
		`SELECT user_id, theme, notifications_enabled, sound_enabled, compact_mode, settings_json, updated_at
		FROM user_settings WHERE user_id = $1`,
		userID,
	).Scan(
		&settings.UserID, &settings.Theme, &settings.NotificationsEnabled,
		&settings.SoundEnabled, &settings.CompactMode, &settings.SettingsJSON, &settings.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Create default settings
			settings = &models.UserSettings{
				UserID:               userID,
				Theme:                "dark",
				NotificationsEnabled: true,
				SoundEnabled:         true,
				CompactMode:          false,
				SettingsJSON:         json.RawMessage("{}"),
			}
			_, err = s.db.Exec(ctx,
				`INSERT INTO user_settings (user_id, theme, notifications_enabled, sound_enabled, compact_mode, settings_json)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				settings.UserID, settings.Theme, settings.NotificationsEnabled,
				settings.SoundEnabled, settings.CompactMode, settings.SettingsJSON,
			)
			if err != nil {
				return nil, err
			}
			return settings, nil
		}
		return nil, err
	}
	if settings.SettingsJSON == nil {
		settings.SettingsJSON = json.RawMessage("{}")
	}
	return settings, nil
}

type UpdateSettingsRequest struct {
	Theme                *string         `json:"theme" validate:"omitempty,oneof=dark light"`
	NotificationsEnabled *bool           `json:"notificationsEnabled"`
	SoundEnabled         *bool           `json:"soundEnabled"`
	CompactMode          *bool           `json:"compactMode"`
	SettingsJSON         json.RawMessage `json:"settings"`
}

func (s *Service) UpdateSettings(ctx context.Context, userID uuid.UUID, req *UpdateSettingsRequest) (*models.UserSettings, error) {
	// Build dynamic update query
	query := `UPDATE user_settings SET updated_at = NOW()`
	args := []interface{}{userID}
	argNum := 2

	if req.Theme != nil {
		query += `, theme = $` + string(rune('0'+argNum))
		args = append(args, *req.Theme)
		argNum++
	}
	if req.NotificationsEnabled != nil {
		query += `, notifications_enabled = $` + string(rune('0'+argNum))
		args = append(args, *req.NotificationsEnabled)
		argNum++
	}
	if req.SoundEnabled != nil {
		query += `, sound_enabled = $` + string(rune('0'+argNum))
		args = append(args, *req.SoundEnabled)
		argNum++
	}
	if req.CompactMode != nil {
		query += `, compact_mode = $` + string(rune('0'+argNum))
		args = append(args, *req.CompactMode)
		argNum++
	}
	if req.SettingsJSON != nil {
		query += `, settings_json = $` + string(rune('0'+argNum))
		args = append(args, req.SettingsJSON)
		argNum++
	}

	query += ` WHERE user_id = $1`

	_, err := s.db.Exec(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	return s.GetSettings(ctx, userID)
}

func sortedFriendPair(first, second uuid.UUID) (uuid.UUID, uuid.UUID) {
	if strings.Compare(first.String(), second.String()) < 0 {
		return first, second
	}
	return second, first
}

func (s *Service) userExists(ctx context.Context, userID uuid.UUID) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`,
		userID,
	).Scan(&exists)
	return exists, err
}

func (s *Service) areFriends(ctx context.Context, userID, otherUserID uuid.UUID) (bool, error) {
	first, second := sortedFriendPair(userID, otherUserID)

	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_friendships WHERE user_id = $1 AND friend_id = $2)`,
		first, second,
	).Scan(&exists)
	return exists, err
}

func (s *Service) hasFriendRequest(ctx context.Context, senderID, receiverID uuid.UUID) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM friend_requests WHERE sender_id = $1 AND receiver_id = $2)`,
		senderID, receiverID,
	).Scan(&exists)
	return exists, err
}

func (s *Service) notifyFriendStateUpdate(ctx context.Context, userIDs ...uuid.UUID) {
	affected := make([]string, 0, len(userIDs))
	seen := make(map[string]struct{}, len(userIDs))

	for _, userID := range userIDs {
		if userID == uuid.Nil {
			continue
		}
		id := userID.String()
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		affected = append(affected, id)
	}

	if len(affected) == 0 {
		return
	}

	s.broadcast(ctx, uuid.Nil, "FRIEND_STATE_UPDATE", map[string]interface{}{
		"affectedUserIds": affected,
	})
}

// Friend system

func (s *Service) SendFriendRequest(ctx context.Context, senderID, receiverID uuid.UUID) error {
	if senderID == receiverID {
		return ErrCannotFriendYourself
	}

	exists, err := s.userExists(ctx, receiverID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrUserNotFound
	}

	if blocked, err := s.IsBlocked(ctx, senderID, receiverID); err != nil {
		return err
	} else if blocked {
		return ErrUsersBlocked
	}

	if blocked, err := s.IsBlocked(ctx, receiverID, senderID); err != nil {
		return err
	} else if blocked {
		return ErrUsersBlocked
	}

	if friends, err := s.areFriends(ctx, senderID, receiverID); err != nil {
		return err
	} else if friends {
		return ErrAlreadyFriends
	}

	if hasRequest, err := s.hasFriendRequest(ctx, senderID, receiverID); err != nil {
		return err
	} else if hasRequest {
		return ErrFriendRequestExists
	}

	if hasRequest, err := s.hasFriendRequest(ctx, receiverID, senderID); err != nil {
		return err
	} else if hasRequest {
		return ErrIncomingFriendRequest
	}

	result, err := s.db.Exec(ctx,
		`INSERT INTO friend_requests (sender_id, receiver_id, created_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (sender_id, receiver_id) DO NOTHING`,
		senderID, receiverID,
	)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrFriendRequestExists
	}

	s.notifyFriendStateUpdate(ctx, senderID, receiverID)
	return nil
}

func (s *Service) AcceptFriendRequest(ctx context.Context, receiverID, senderID uuid.UUID) error {
	if receiverID == senderID {
		return ErrCannotAcceptOwnRequest
	}

	exists, err := s.userExists(ctx, senderID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrUserNotFound
	}

	if blocked, err := s.IsBlocked(ctx, receiverID, senderID); err != nil {
		return err
	} else if blocked {
		return ErrUsersBlocked
	}

	if blocked, err := s.IsBlocked(ctx, senderID, receiverID); err != nil {
		return err
	} else if blocked {
		return ErrUsersBlocked
	}

	first, second := sortedFriendPair(receiverID, senderID)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	result, err := tx.Exec(ctx,
		`DELETE FROM friend_requests WHERE sender_id = $1 AND receiver_id = $2`,
		senderID, receiverID,
	)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrFriendRequestNotFound
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO user_friendships (user_id, friend_id, created_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (user_id, friend_id) DO NOTHING`,
		first, second,
	)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	s.notifyFriendStateUpdate(ctx, receiverID, senderID)
	return nil
}

func (s *Service) RemoveFriendRequest(ctx context.Context, userID, otherUserID uuid.UUID) error {
	if userID == otherUserID {
		return ErrCannotRemoveSelfRequest
	}

	exists, err := s.userExists(ctx, otherUserID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrUserNotFound
	}

	result, err := s.db.Exec(ctx,
		`DELETE FROM friend_requests
		 WHERE (sender_id = $1 AND receiver_id = $2)
		    OR (sender_id = $2 AND receiver_id = $1)`,
		userID, otherUserID,
	)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrFriendRequestNotFound
	}

	s.notifyFriendStateUpdate(ctx, userID, otherUserID)
	return nil
}

func (s *Service) GetFriendRequests(ctx context.Context, userID uuid.UUID) (*models.FriendRequests, error) {
	requests := &models.FriendRequests{
		Incoming: make([]*models.FriendRequest, 0),
		Outgoing: make([]*models.FriendRequest, 0),
	}

	incomingRows, err := s.db.Query(ctx,
		`SELECT fr.created_at, u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		 FROM friend_requests fr
		 JOIN users u ON u.id = fr.sender_id
		 WHERE fr.receiver_id = $1 AND u.deleted_at IS NULL
		 ORDER BY fr.created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer incomingRows.Close()

	for incomingRows.Next() {
		friendUser := &models.PublicUser{}
		request := &models.FriendRequest{User: friendUser}
		if err := incomingRows.Scan(
			&request.CreatedAt,
			&friendUser.ID, &friendUser.Username, &friendUser.DisplayName, &friendUser.AvatarURL,
			&friendUser.Bio, &friendUser.Status, &friendUser.CustomStatus, &friendUser.CreatedAt,
		); err != nil {
			return nil, err
		}
		requests.Incoming = append(requests.Incoming, request)
	}

	outgoingRows, err := s.db.Query(ctx,
		`SELECT fr.created_at, u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		 FROM friend_requests fr
		 JOIN users u ON u.id = fr.receiver_id
		 WHERE fr.sender_id = $1 AND u.deleted_at IS NULL
		 ORDER BY fr.created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer outgoingRows.Close()

	for outgoingRows.Next() {
		friendUser := &models.PublicUser{}
		request := &models.FriendRequest{User: friendUser}
		if err := outgoingRows.Scan(
			&request.CreatedAt,
			&friendUser.ID, &friendUser.Username, &friendUser.DisplayName, &friendUser.AvatarURL,
			&friendUser.Bio, &friendUser.Status, &friendUser.CustomStatus, &friendUser.CreatedAt,
		); err != nil {
			return nil, err
		}
		requests.Outgoing = append(requests.Outgoing, request)
	}

	return requests, nil
}

func (s *Service) GetFriends(ctx context.Context, userID uuid.UUID) ([]*models.PublicUser, error) {
	rows, err := s.db.Query(ctx,
		`SELECT u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		 FROM user_friendships f
		 JOIN users u ON u.id = CASE WHEN f.user_id = $1 THEN f.friend_id ELSE f.user_id END
		 WHERE (f.user_id = $1 OR f.friend_id = $1)
		   AND u.deleted_at IS NULL
		 ORDER BY COALESCE(u.display_name, u.username), u.username`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	friends := make([]*models.PublicUser, 0)
	for rows.Next() {
		friendUser := &models.PublicUser{}
		if err := rows.Scan(
			&friendUser.ID, &friendUser.Username, &friendUser.DisplayName, &friendUser.AvatarURL,
			&friendUser.Bio, &friendUser.Status, &friendUser.CustomStatus, &friendUser.CreatedAt,
		); err != nil {
			return nil, err
		}
		friends = append(friends, friendUser)
	}

	return friends, nil
}

func (s *Service) RemoveFriend(ctx context.Context, userID, friendID uuid.UUID) error {
	if userID == friendID {
		return ErrCannotRemoveSelfFriend
	}

	exists, err := s.userExists(ctx, friendID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrUserNotFound
	}

	first, second := sortedFriendPair(userID, friendID)

	result, err := s.db.Exec(ctx,
		`DELETE FROM user_friendships WHERE user_id = $1 AND friend_id = $2`,
		first, second,
	)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFriends
	}

	s.notifyFriendStateUpdate(ctx, userID, friendID)
	return nil
}

func (s *Service) GetRelationship(ctx context.Context, userID, otherUserID uuid.UUID) (*models.UserRelationship, error) {
	if userID == otherUserID {
		return &models.UserRelationship{UserID: otherUserID, Status: models.UserRelationshipNone}, nil
	}

	exists, err := s.userExists(ctx, otherUserID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrUserNotFound
	}

	if blocked, err := s.IsBlocked(ctx, userID, otherUserID); err != nil {
		return nil, err
	} else if blocked {
		return &models.UserRelationship{UserID: otherUserID, Status: models.UserRelationshipBlocked}, nil
	}

	if blocked, err := s.IsBlocked(ctx, otherUserID, userID); err != nil {
		return nil, err
	} else if blocked {
		return &models.UserRelationship{UserID: otherUserID, Status: models.UserRelationshipBlockedBy}, nil
	}

	if friends, err := s.areFriends(ctx, userID, otherUserID); err != nil {
		return nil, err
	} else if friends {
		return &models.UserRelationship{UserID: otherUserID, Status: models.UserRelationshipFriends}, nil
	}

	if hasRequest, err := s.hasFriendRequest(ctx, userID, otherUserID); err != nil {
		return nil, err
	} else if hasRequest {
		return &models.UserRelationship{UserID: otherUserID, Status: models.UserRelationshipOutgoingRequest}, nil
	}

	if hasRequest, err := s.hasFriendRequest(ctx, otherUserID, userID); err != nil {
		return nil, err
	} else if hasRequest {
		return &models.UserRelationship{UserID: otherUserID, Status: models.UserRelationshipIncomingRequest}, nil
	}

	return &models.UserRelationship{UserID: otherUserID, Status: models.UserRelationshipNone}, nil
}

// Blocking

func (s *Service) BlockUser(ctx context.Context, blockerID, blockedID uuid.UUID) error {
	if blockerID == blockedID {
		return errors.New("cannot block yourself")
	}

	exists, err := s.userExists(ctx, blockedID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrUserNotFound
	}

	first, second := sortedFriendPair(blockerID, blockedID)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx,
		`INSERT INTO user_blocks (blocker_id, blocked_id) VALUES ($1, $2)
		ON CONFLICT (blocker_id, blocked_id) DO NOTHING`,
		blockerID, blockedID,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx,
		`DELETE FROM user_friendships WHERE user_id = $1 AND friend_id = $2`,
		first, second,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx,
		`DELETE FROM friend_requests
		 WHERE (sender_id = $1 AND receiver_id = $2)
		    OR (sender_id = $2 AND receiver_id = $1)`,
		blockerID, blockedID,
	)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	s.notifyFriendStateUpdate(ctx, blockerID, blockedID)
	return nil
}

func (s *Service) UnblockUser(ctx context.Context, blockerID, blockedID uuid.UUID) error {
	result, err := s.db.Exec(ctx,
		`DELETE FROM user_blocks WHERE blocker_id = $1 AND blocked_id = $2`,
		blockerID, blockedID,
	)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotBlocked
	}
	return nil
}

func (s *Service) GetBlockedUsers(ctx context.Context, userID uuid.UUID) ([]*models.PublicUser, error) {
	rows, err := s.db.Query(ctx,
		`SELECT u.id, u.username, u.display_name, u.avatar_url, u.bio, u.status, u.custom_status, u.created_at
		FROM user_blocks b
		JOIN users u ON u.id = b.blocked_id
		WHERE b.blocker_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []*models.PublicUser
	for rows.Next() {
		user := &models.PublicUser{}
		err := rows.Scan(
			&user.ID, &user.Username, &user.DisplayName, &user.AvatarURL,
			&user.Bio, &user.Status, &user.CustomStatus, &user.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}

	return users, nil
}

func (s *Service) IsBlocked(ctx context.Context, blockerID, blockedID uuid.UUID) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_blocks WHERE blocker_id = $1 AND blocked_id = $2)`,
		blockerID, blockedID,
	).Scan(&exists)
	return exists, err
}
