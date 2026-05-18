package admin

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

func (s *Service) GetAdmins(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, username, avatar_url, created_at FROM users WHERE is_admin = TRUE AND deleted_at IS NULL ORDER BY created_at ASC`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to query admins")
		return nil, err
	}
	defer rows.Close()

	var admins []AdminUser
	for rows.Next() {
		var a AdminUser
		if err := rows.Scan(&a.ID, &a.Username, &a.AvatarURL, &a.CreatedAt); err != nil {
			log.Warn().Err(err).Msg("Failed to scan admin row")
			return nil, err
		}
		admins = append(admins, a)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating admin rows")
		return nil, err
	}

	return admins, nil
}

func (s *Service) AddAdmin(ctx context.Context, actorID, targetUserID uuid.UUID) error {
	if actorID == targetUserID {
		return errors.New("you are already an admin")
	}

	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`,
		targetUserID,
	).Scan(&exists)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to check if user exists")
		return err
	}
	if !exists {
		return ErrUserNotFound
	}

	var isAdmin bool
	err = s.db.QueryRow(ctx,
		`SELECT is_admin FROM users WHERE id = $1`,
		targetUserID,
	).Scan(&isAdmin)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to check if user is admin")
		return err
	}
	if isAdmin {
		return ErrAlreadyAdmin
	}

	_, err = s.db.Exec(ctx,
		`UPDATE users SET is_admin = TRUE WHERE id = $1`,
		targetUserID,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to update user to admin")
		return err
	}

	return nil
}

func (s *Service) RemoveAdmin(ctx context.Context, actorID, targetUserID uuid.UUID) error {
	if actorID == targetUserID {
		return ErrCannotRemoveSelf
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
		log.Warn().Err(err).Msg("Failed to check admin status")
		return err
	}
	if !isAdmin {
		return ErrNotAdmin
	}

	var adminCount int64
	err = s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE is_admin = TRUE AND deleted_at IS NULL`,
	).Scan(&adminCount)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to count admins")
		return err
	}
	if adminCount <= 1 {
		return ErrLastAdmin
	}

	_, err = s.db.Exec(ctx,
		`UPDATE users SET is_admin = FALSE WHERE id = $1`,
		targetUserID,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to remove admin")
		return err
	}

	return nil
}

func (s *Service) EnsureFirstUserIsAdmin(ctx context.Context) error {
	var adminExists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE is_admin = TRUE AND deleted_at IS NULL)`,
	).Scan(&adminExists)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to check if admin exists")
		return err
	}

	if adminExists {
		return nil
	}

	_, err = s.db.Exec(ctx,
		`UPDATE users SET is_admin = TRUE WHERE id = (SELECT id FROM users WHERE deleted_at IS NULL ORDER BY created_at ASC LIMIT 1)`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to promote first user to admin")
		return err
	}

	log.Info().Msg("First user automatically promoted to admin")
	return nil
}
