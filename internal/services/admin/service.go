package admin

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

var (
	ErrNotAdmin          = errors.New("user is not an admin")
	ErrAlreadyAdmin      = errors.New("user is already an admin")
	ErrCannotRemoveSelf  = errors.New("cannot remove yourself as admin")
	ErrUserNotFound      = errors.New("user not found")
	ErrLastAdmin         = errors.New("cannot remove the last admin")
)

type Service struct {
	db *pgxpool.Pool
}

func NewService(db *pgxpool.Pool) *Service {
	return &Service{
		db: db,
	}
}

// DataPoint represents a metric measurement at a point in time
type DataPoint struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}

// DashboardStats holds the aggregated instance statistics
type DashboardStats struct {
	TotalUsers      int64       `json:"totalUsers"`
	TotalMessages   int64       `json:"totalMessages"`
	TotalCommunities int64      `json:"totalCommunities"`
	UsersOverTime   []DataPoint `json:"usersOverTime"`
	MessagesOverTime []DataPoint `json:"messagesOverTime"`
	CommunitiesOverTime []DataPoint `json:"communitiesOverTime"`
}

// AdminUser is a public representation of an admin user
type AdminUser struct {
	ID        string  `json:"id"`
	Username  string  `json:"username"`
	AvatarURL *string `json:"avatarUrl,omitempty"`
	CreatedAt string  `json:"createdAt"`
}

func (s *Service) GetDashboard(ctx context.Context) (*DashboardStats, error) {
	stats := &DashboardStats{}

	// Total users
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL`).Scan(&stats.TotalUsers)
	if err != nil {
		return nil, err
	}

	// Total messages
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages`).Scan(&stats.TotalMessages)
	if err != nil {
		return nil, err
	}

	// Total communities
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM communities WHERE deleted_at IS NULL`).Scan(&stats.TotalCommunities)
	if err != nil {
		return nil, err
	}

	// Users over time (last 30 days)
	stats.UsersOverTime, err = s.getCountOverTime(ctx,
		`SELECT DATE(created_at)::text, COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '30 days' GROUP BY DATE(created_at) ORDER BY DATE(created_at)`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch users over time")
	}

	// Messages over time (last 30 days)
	stats.MessagesOverTime, err = s.getCountOverTime(ctx,
		`SELECT DATE(created_at)::text, COUNT(*) FROM messages WHERE created_at >= NOW() - INTERVAL '30 days' GROUP BY DATE(created_at) ORDER BY DATE(created_at)`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch messages over time")
	}

	// Communities over time (last 30 days)
	stats.CommunitiesOverTime, err = s.getCountOverTime(ctx,
		`SELECT DATE(created_at)::text, COUNT(*) FROM communities WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '30 days' GROUP BY DATE(created_at) ORDER BY DATE(created_at)`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch communities over time")
	}

	return stats, nil
}

func (s *Service) getCountOverTime(ctx context.Context, query string) ([]DataPoint, error) {
	rows, err := s.db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var points []DataPoint
	for rows.Next() {
		var dp DataPoint
		if err := rows.Scan(&dp.Date, &dp.Count); err != nil {
			return nil, err
		}
		points = append(points, dp)
	}

	points = fillMissingDays(points, 30)

	return points, nil
}

func fillMissingDays(points []DataPoint, days int) []DataPoint {
	now := time.Now().Truncate(24 * time.Hour)
	pointMap := make(map[string]int64)
	for _, p := range points {
		pointMap[p.Date] = p.Count
	}

	filled := make([]DataPoint, 0, days)
	for i := days - 1; i >= 0; i-- {
		date := now.AddDate(0, 0, -i).Format("2006-01-02")
		count := pointMap[date]
		filled = append(filled, DataPoint{Date: date, Count: count})
	}

	return filled
}

func (s *Service) GetAdmins(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, username, avatar_url, created_at FROM users WHERE is_admin = TRUE AND deleted_at IS NULL ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var admins []AdminUser
	for rows.Next() {
		var a AdminUser
		if err := rows.Scan(&a.ID, &a.Username, &a.AvatarURL, &a.CreatedAt); err != nil {
			return nil, err
		}
		admins = append(admins, a)
	}

	return admins, nil
}

func (s *Service) AddAdmin(ctx context.Context, actorID, targetUserID uuid.UUID) error {
	if actorID == targetUserID {
		return errors.New("you are already an admin")
	}

	// Check target user exists
	var exists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`,
		targetUserID,
	).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return ErrUserNotFound
	}

	// Check if already admin
	var isAdmin bool
	err = s.db.QueryRow(ctx,
		`SELECT is_admin FROM users WHERE id = $1`,
		targetUserID,
	).Scan(&isAdmin)
	if err != nil {
		return err
	}
	if isAdmin {
		return ErrAlreadyAdmin
	}

	_, err = s.db.Exec(ctx,
		`UPDATE users SET is_admin = TRUE WHERE id = $1`,
		targetUserID,
	)
	return err
}

func (s *Service) RemoveAdmin(ctx context.Context, actorID, targetUserID uuid.UUID) error {
	if actorID == targetUserID {
		return ErrCannotRemoveSelf
	}

	// Check target user exists and is admin
	var isAdmin bool
	err := s.db.QueryRow(ctx,
		`SELECT is_admin FROM users WHERE id = $1 AND deleted_at IS NULL`,
		targetUserID,
	).Scan(&isAdmin)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUserNotFound
		}
		return err
	}
	if !isAdmin {
		return ErrNotAdmin
	}

	// Check this is not the last admin
	var adminCount int64
	err = s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE is_admin = TRUE AND deleted_at IS NULL`,
	).Scan(&adminCount)
	if err != nil {
		return err
	}
	if adminCount <= 1 {
		return ErrLastAdmin
	}

	_, err = s.db.Exec(ctx,
		`UPDATE users SET is_admin = FALSE WHERE id = $1`,
		targetUserID,
	)
	return err
}

// EnsureFirstUserIsAdmin checks if any admin exists and if not, makes the first user an admin
func (s *Service) EnsureFirstUserIsAdmin(ctx context.Context) error {
	var adminExists bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE is_admin = TRUE AND deleted_at IS NULL)`,
	).Scan(&adminExists)
	if err != nil {
		return err
	}

	if adminExists {
		return nil
	}

	// No admin exists - make the first registered user the admin
	_, err = s.db.Exec(ctx,
		`UPDATE users SET is_admin = TRUE WHERE id = (SELECT id FROM users WHERE deleted_at IS NULL ORDER BY created_at ASC LIMIT 1)`,
	)
	if err != nil {
		return err
	}

	log.Info().Msg("First user automatically promoted to admin")
	return nil
}
