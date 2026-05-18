package admin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

func (s *Service) ListUsers(ctx context.Context, page, pageSize int, query, status string) ([]AdminUserListItem, int64, error) {
	offset := (page - 1) * pageSize
	args := []any{}
	argIdx := 1

	where := `WHERE deleted_at IS NULL`

	if query != "" {
		searchPattern := "%" + query + "%"
		where += fmt.Sprintf(` AND ($%d = '' OR username ILIKE $%d OR COALESCE(display_name, '') ILIKE $%d OR email ILIKE $%d)`, argIdx, argIdx, argIdx, argIdx)
		args = append(args, searchPattern)
		argIdx++
	}

	if status != "" {
		where += fmt.Sprintf(` AND status = $%d::user_status`, argIdx)
		args = append(args, status)
		argIdx++
	}

	var total int64
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM users %s`, where)
	err := s.db.QueryRow(ctx, countQuery, args...).Scan(&total)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to count users for admin list")
		return nil, 0, err
	}

	listQuery := fmt.Sprintf(`SELECT id, username, display_name, avatar_url, email, email_verified,
	       status, is_admin, created_at, last_seen_at, deleted_at
	FROM users %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`, where, argIdx, argIdx+1)
	args = append(args, pageSize, offset)

	rows, err := s.db.Query(ctx, listQuery, args...)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to query users for admin list")
		return nil, 0, err
	}
	defer rows.Close()

	var users []AdminUserListItem
	for rows.Next() {
		var u AdminUserListItem
		var id uuid.UUID
		if err := rows.Scan(&id, &u.Username, &u.DisplayName, &u.AvatarURL, &u.Email,
			&u.EmailVerified, &u.Status, &u.IsAdmin, &u.CreatedAt, &u.LastSeenAt, &u.DeletedAt); err != nil {
			log.Warn().Err(err).Msg("Failed to scan admin user list row")
			continue
		}
		u.ID = id.String()
		users = append(users, u)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating admin user list rows")
		return nil, 0, err
	}

	if users == nil {
		users = []AdminUserListItem{}
	}

	return users, total, nil
}

func (s *Service) GetUser(ctx context.Context, targetUserID uuid.UUID) (*AdminUserDetail, error) {
	query := `
		SELECT id, username, email, display_name, avatar_url, bio, status, custom_status,
		       email_verified, two_factor_enabled, is_admin, created_at, updated_at, last_seen_at, deleted_at
		FROM users
		WHERE id = $1
	`
	var u AdminUserDetail
	var id uuid.UUID
	err := s.db.QueryRow(ctx, query, targetUserID).Scan(
		&id, &u.Username, &u.Email, &u.DisplayName, &u.AvatarURL, &u.Bio,
		&u.Status, &u.CustomStatus, &u.EmailVerified, &u.TwoFactorEnabled,
		&u.IsAdmin, &u.CreatedAt, &u.UpdatedAt, &u.LastSeenAt, &u.DeletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		log.Warn().Err(err).Msg("Failed to get user for admin detail")
		return nil, err
	}
	u.ID = id.String()

	return &u, nil
}

func (s *Service) UpdateUser(ctx context.Context, actorID, targetUserID uuid.UUID, req *UpdateUserRequest) (*AdminUserDetail, error) {
	if req.Username != nil {
		*req.Username = strings.TrimSpace(*req.Username)
		if len(*req.Username) < 3 || len(*req.Username) > 32 {
			return nil, errors.New("username must be 3-32 characters")
		}
		usernameRegex := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
		if !usernameRegex.MatchString(*req.Username) {
			return nil, errors.New("username contains invalid characters")
		}

		var exists bool
		err := s.db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE username = $1 AND id != $2 AND deleted_at IS NULL)`,
			*req.Username, targetUserID,
		).Scan(&exists)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to check username availability")
			return nil, err
		}
		if exists {
			return nil, ErrUsernameTaken
		}
	}

	if req.Email != nil {
		*req.Email = strings.TrimSpace(*req.Email)
		if *req.Email == "" {
			return nil, errors.New("email cannot be empty")
		}

		var exists bool
		err := s.db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE email = $1 AND id != $2 AND deleted_at IS NULL)`,
			*req.Email, targetUserID,
		).Scan(&exists)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to check email availability")
			return nil, err
		}
		if exists {
			return nil, ErrEmailTaken
		}
	}

	query := `UPDATE users SET updated_at = NOW()`
	args := []any{}
	argIdx := 1

	if req.Username != nil {
		query += fmt.Sprintf(", username = $%d", argIdx)
		args = append(args, *req.Username)
		argIdx++
	}
	if req.DisplayName != nil {
		query += fmt.Sprintf(", display_name = $%d", argIdx)
		args = append(args, *req.DisplayName)
		argIdx++
	}
	if req.Bio != nil {
		query += fmt.Sprintf(", bio = $%d", argIdx)
		args = append(args, *req.Bio)
		argIdx++
	}
	if req.Email != nil {
		query += fmt.Sprintf(", email = $%d", argIdx)
		args = append(args, *req.Email)
		argIdx++
	}
	if req.EmailVerified != nil {
		query += fmt.Sprintf(", email_verified = $%d", argIdx)
		args = append(args, *req.EmailVerified)
		argIdx++
	}
	if req.CustomStatus != nil {
		query += fmt.Sprintf(", custom_status = $%d", argIdx)
		args = append(args, *req.CustomStatus)
		argIdx++
	}

	query += fmt.Sprintf(" WHERE id = $%d AND deleted_at IS NULL", argIdx)
	args = append(args, targetUserID)

	result, err := s.db.Exec(ctx, query, args...)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to update user")
		return nil, err
	}

	if result.RowsAffected() == 0 {
		return nil, ErrUserNotFound
	}

	return s.GetUser(ctx, targetUserID)
}

func (s *Service) DeleteUser(ctx context.Context, actorID, targetUserID uuid.UUID) error {
	if actorID == targetUserID {
		return ErrCannotDeleteSelf
	}

	var isAdmin bool
	err := s.db.QueryRow(ctx,
		`SELECT is_admin FROM users WHERE id = $1 AND deleted_at IS NULL`,
		targetUserID,
	).Scan(&isAdmin)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		log.Warn().Err(err).Msg("Failed to check target user for deletion")
		return err
	}
	if isAdmin {
		return ErrCannotDeleteAdmin
	}

	_, err = s.db.Exec(ctx,
		`UPDATE users SET deleted_at = NOW(), status = 'offline' WHERE id = $1 AND deleted_at IS NULL`,
		targetUserID,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to soft delete user")
		return err
	}

	_, err = s.db.Exec(ctx,
		`UPDATE user_sessions SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`,
		targetUserID,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to revoke user sessions on delete")
	}

	return nil
}

func (s *Service) RestoreUser(ctx context.Context, targetUserID uuid.UUID) error {
	var deletedAt *time.Time
	err := s.db.QueryRow(ctx,
		`SELECT deleted_at FROM users WHERE id = $1`,
		targetUserID,
	).Scan(&deletedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		log.Warn().Err(err).Msg("Failed to check user for restore")
		return err
	}
	if deletedAt == nil {
		return ErrUserNotDeleted
	}

	_, err = s.db.Exec(ctx,
		`UPDATE users SET deleted_at = NULL WHERE id = $1`,
		targetUserID,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to restore user")
		return err
	}

	return nil
}
