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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

var (
	ErrNotAdmin           = errors.New("user is not an admin")
	ErrAlreadyAdmin       = errors.New("user is already an admin")
	ErrCannotRemoveSelf   = errors.New("cannot remove yourself as admin")
	ErrUserNotFound       = errors.New("user not found")
	ErrLastAdmin          = errors.New("cannot remove the last admin")
	ErrUserNotDeleted     = errors.New("user is not deleted")
	ErrUsernameTaken      = errors.New("username is already taken")
	ErrEmailTaken         = errors.New("email is already taken")
	ErrCannotDeleteSelf   = errors.New("cannot delete yourself")
	ErrCannotDeleteAdmin  = errors.New("cannot delete an admin user")
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

// AnalyticsStats holds the comprehensive instance analytics
type AnalyticsStats struct {
	TotalUsers       int64       `json:"totalUsers"`
	TotalMessages    int64       `json:"totalMessages"`
	TotalCommunities int64       `json:"totalCommunities"`
	TotalChannels    int64       `json:"totalChannels"`
	OnlineUsers      int64       `json:"onlineUsers"`
	MessagesToday    int64       `json:"messagesToday"`
	NewUsersToday    int64       `json:"newUsersToday"`
	ActiveUsers7d    int64       `json:"activeUsers7d"`
	NewUsers7d       int64       `json:"newUsers7d"`
	NewUsers30d      int64       `json:"newUsers30d"`
	Messages7d       int64       `json:"messages7d"`
	Messages30d      int64       `json:"messages30d"`
	AvgMessagesPerUser     float64 `json:"avgMessagesPerUser"`
	AvgMembersPerCommunity  float64 `json:"avgMembersPerCommunity"`
	UserGrowthRate         float64 `json:"userGrowthRate"`
	MessageGrowthRate      float64 `json:"messageGrowthRate"`
	CommunityGrowthRate    float64 `json:"communityGrowthRate"`
	UsersOverTime          []DataPoint `json:"usersOverTime"`
	MessagesOverTime       []DataPoint `json:"messagesOverTime"`
	CommunitiesOverTime    []DataPoint `json:"communitiesOverTime"`
	ActiveUsersOverTime    []DataPoint `json:"activeUsersOverTime"`
	TopCommunities         []CommunityStat `json:"topCommunities"`
	ActiveHours            []HourlyStat   `json:"activeHours"`
	ActiveWeekdays         []DailyStat    `json:"activeWeekdays"`
}

type CommunityStat struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MemberCount  int    `json:"memberCount"`
	MessageCount int64  `json:"messageCount"`
	CreatedAt    string `json:"createdAt"`
}

type HourlyStat struct {
	Hour  string `json:"hour"`
	Count int64  `json:"count"`
}

type DailyStat struct {
	Day   string `json:"day"`
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
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	AvatarURL *string   `json:"avatarUrl,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// AdminUserListItem is a summary view of a user for the admin user list
type AdminUserListItem struct {
	ID            string     `json:"id"`
	Username      string     `json:"username"`
	DisplayName   *string    `json:"displayName,omitempty"`
	AvatarURL     *string    `json:"avatarUrl,omitempty"`
	Email         string     `json:"email"`
	EmailVerified bool       `json:"emailVerified"`
	Status        string     `json:"status"`
	IsAdmin       bool       `json:"isAdmin"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastSeenAt    *time.Time `json:"lastSeenAt,omitempty"`
	DeletedAt     *time.Time `json:"deletedAt,omitempty"`
}

// AdminUserDetail is the full user detail for the admin user detail view
type AdminUserDetail struct {
	ID               string     `json:"id"`
	Username         string     `json:"username"`
	Email            string     `json:"email"`
	DisplayName      *string    `json:"displayName,omitempty"`
	AvatarURL        *string    `json:"avatarUrl,omitempty"`
	Bio              *string    `json:"bio,omitempty"`
	Status           string     `json:"status"`
	CustomStatus     *string    `json:"customStatus,omitempty"`
	EmailVerified    bool       `json:"emailVerified"`
	TwoFactorEnabled bool       `json:"twoFactorEnabled"`
	IsAdmin          bool       `json:"isAdmin"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	LastSeenAt       *time.Time `json:"lastSeenAt,omitempty"`
	DeletedAt        *time.Time `json:"deletedAt,omitempty"`
}

// UpdateUserRequest is the request body for updating a user as admin
type UpdateUserRequest struct {
	Username      *string `json:"username,omitempty"`
	DisplayName   *string `json:"displayName,omitempty"`
	Bio           *string `json:"bio,omitempty"`
	Email         *string `json:"email,omitempty"`
	EmailVerified *bool   `json:"emailVerified,omitempty"`
	CustomStatus  *string `json:"customStatus,omitempty"`
}

func (s *Service) GetDashboard(ctx context.Context) (*DashboardStats, error) {
	stats := &DashboardStats{}

	// Total users
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL`).Scan(&stats.TotalUsers)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total users")
		return nil, err
	}

	// Total messages
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages`).Scan(&stats.TotalMessages)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total messages")
		return nil, err
	}

	// Total communities
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM communities WHERE deleted_at IS NULL`).Scan(&stats.TotalCommunities)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total communities")
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

func (s *Service) GetAnalytics(ctx context.Context) (*AnalyticsStats, error) {
	stats := &AnalyticsStats{}

	// Total users
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL`).Scan(&stats.TotalUsers); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total users")
	}
	// Total messages
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages`).Scan(&stats.TotalMessages); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total messages")
	}
	// Total communities
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM communities WHERE deleted_at IS NULL`).Scan(&stats.TotalCommunities); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total communities")
	}
	// Total channels
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM channels`).Scan(&stats.TotalChannels); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total channels")
	}

	// Online users
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE status = 'online' AND deleted_at IS NULL`).Scan(&stats.OnlineUsers); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch online users")
	}

	// Messages today
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= CURRENT_DATE`).Scan(&stats.MessagesToday); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch messages today")
	}

	// New users today
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= CURRENT_DATE`).Scan(&stats.NewUsersToday); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch new users today")
	}

	// Active users in the last 7 days (users who sent a message)
	if err := s.db.QueryRow(ctx, `SELECT COUNT(DISTINCT author_id) FROM messages WHERE created_at >= NOW() - INTERVAL '7 days'`).Scan(&stats.ActiveUsers7d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch active users 7d")
	}

	// New users in the last 7 days
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '7 days'`).Scan(&stats.NewUsers7d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch new users 7d")
	}

	// New users in the last 30 days
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '30 days'`).Scan(&stats.NewUsers30d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch new users 30d")
	}

	// Messages in the last 7 days
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= NOW() - INTERVAL '7 days'`).Scan(&stats.Messages7d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch messages 7d")
	}

	// Messages in the last 30 days
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= NOW() - INTERVAL '30 days'`).Scan(&stats.Messages30d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch messages 30d")
	}

	// Average messages per user
	if stats.TotalUsers > 0 {
		stats.AvgMessagesPerUser = float64(stats.TotalMessages) / float64(stats.TotalUsers)
	}

	// Average members per community (only communities with members)
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(AVG(member_count), 0) FROM communities WHERE deleted_at IS NULL AND member_count > 0`).Scan(&stats.AvgMembersPerCommunity); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch avg members per community")
	}

	// Growth rates (compare last 7 days to previous 7 days)
	var prev7Users, prev7Messages, prev7Communities int64
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '14 days' AND created_at < NOW() - INTERVAL '7 days'`).Scan(&prev7Users); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch prev7 users")
	}
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= NOW() - INTERVAL '14 days' AND created_at < NOW() - INTERVAL '7 days'`).Scan(&prev7Messages); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch prev7 messages")
	}
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM communities WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '14 days' AND created_at < NOW() - INTERVAL '7 days'`).Scan(&prev7Communities); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch prev7 communities")
	}

	if prev7Users > 0 {
		stats.UserGrowthRate = (float64(stats.NewUsers7d) - float64(prev7Users)) / float64(prev7Users) * 100
	}
	if prev7Messages > 0 {
		stats.MessageGrowthRate = (float64(stats.Messages7d) - float64(prev7Messages)) / float64(prev7Messages) * 100
	}
	var newCommunities7d int64
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM communities WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '7 days'`).Scan(&newCommunities7d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch new communities 7d")
	}
	if prev7Communities > 0 {
		stats.CommunityGrowthRate = (float64(newCommunities7d) - float64(prev7Communities)) / float64(prev7Communities) * 100
	}
	// Communities over time (last 30 days)
	var err error
	stats.UsersOverTime, err = s.getCountOverTime(ctx,
		`SELECT DATE(created_at)::text, COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '30 days' GROUP BY DATE(created_at) ORDER BY DATE(created_at)`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch users over time")
	}

	stats.MessagesOverTime, err = s.getCountOverTime(ctx,
		`SELECT DATE(created_at)::text, COUNT(*) FROM messages WHERE created_at >= NOW() - INTERVAL '30 days' GROUP BY DATE(created_at) ORDER BY DATE(created_at)`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch messages over time")
	}

	stats.CommunitiesOverTime, err = s.getCountOverTime(ctx,
		`SELECT DATE(created_at)::text, COUNT(*) FROM communities WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '30 days' GROUP BY DATE(created_at) ORDER BY DATE(created_at)`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch communities over time")
	}

	// Active users over time (distinct users who sent a message each day)
	stats.ActiveUsersOverTime, err = s.getCountOverTime(ctx,
		`SELECT DATE(created_at)::text, COUNT(DISTINCT author_id) FROM messages WHERE created_at >= NOW() - INTERVAL '30 days' GROUP BY DATE(created_at) ORDER BY DATE(created_at)`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch active users over time")
	}

	// Top communities by member count + message count
	stats.TopCommunities = s.getTopCommunities(ctx)

	// Active hours (messages by hour in the last 30 days)
	stats.ActiveHours = s.getActiveHours(ctx)

	// Active weekdays (messages by day of week in the last 30 days)
	stats.ActiveWeekdays = s.getActiveWeekdays(ctx)

	return stats, nil
}

func (s *Service) getTopCommunities(ctx context.Context) []CommunityStat {
	query := `
		SELECT c.id, c.name, c.member_count,
			COALESCE(msg_counts.msg_count, 0) as message_count,
			c.created_at::text
		FROM communities c
		LEFT JOIN (
			SELECT ch.community_id, COUNT(m.id) as msg_count
			FROM channels ch
			LEFT JOIN messages m ON m.channel_id = ch.id AND m.deleted_at IS NULL
			WHERE ch.community_id IS NOT NULL
			GROUP BY ch.community_id
		) msg_counts ON msg_counts.community_id = c.id
		WHERE c.deleted_at IS NULL
		ORDER BY c.member_count DESC
		LIMIT 10
	`

	rows, err := s.db.Query(ctx, query)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch top communities")
		return nil
	}
	defer rows.Close()

	var communities []CommunityStat
	for rows.Next() {
		var cs CommunityStat
		if err := rows.Scan(&cs.ID, &cs.Name, &cs.MemberCount, &cs.MessageCount, &cs.CreatedAt); err != nil {
			log.Warn().Err(err).Msg("Failed to scan community stat")
			continue
		}
		communities = append(communities, cs)
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating top community rows")
	}

	return communities
}

func (s *Service) getActiveHours(ctx context.Context) []HourlyStat {
	rows, err := s.db.Query(ctx,
		`SELECT EXTRACT(HOUR FROM created_at)::int as hour, COUNT(*) as count
		FROM messages
		WHERE created_at >= NOW() - INTERVAL '30 days'
		GROUP BY EXTRACT(HOUR FROM created_at)
		ORDER BY hour`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch active hours")
		return nil
	}
	defer rows.Close()

	var hours []HourlyStat
	hourMap := make(map[int]int64)
	for rows.Next() {
		var h int
		var c int64
		if err := rows.Scan(&h, &c); err != nil {
			log.Warn().Err(err).Msg("Failed to scan active hour row")
			continue
		}
		hourMap[h] = c
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating active hour rows")
	}

	// Fill all 24 hours with zero counts for missing hours
	for i := 0; i < 24; i++ {
		label := fmt.Sprintf("%02d:00", i)
		hours = append(hours, HourlyStat{Hour: label, Count: hourMap[i]})
	}
	return hours
}

func (s *Service) getActiveWeekdays(ctx context.Context) []DailyStat {
	rows, err := s.db.Query(ctx,
		`SELECT EXTRACT(DOW FROM created_at)::int as dow, COUNT(*) as count
		FROM messages
		WHERE created_at >= NOW() - INTERVAL '30 days'
		GROUP BY EXTRACT(DOW FROM created_at)
		ORDER BY dow`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch active weekdays")
		return nil
	}
	defer rows.Close()

	dayNames := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	dayMap := make(map[int]int64)
	for rows.Next() {
		var d int
		var c int64
		if err := rows.Scan(&d, &c); err != nil {
			log.Warn().Err(err).Msg("Failed to scan active weekday row")
			continue
		}
		dayMap[d] = c
	}

	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Error iterating active weekday rows")
	}

	var days []DailyStat
	for i := 0; i < 7; i++ {
		days = append(days, DailyStat{Day: dayNames[i], Count: dayMap[i]})
	}
	return days
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

	if err := rows.Err(); err != nil {
		return points, err
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

	// Check target user exists
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

	// Check if already admin
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
		log.Warn().Err(err).Msg("Failed to check admin status")
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

// EnsureFirstUserIsAdmin checks if any admin exists and if not, makes the first user an admin
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

	// No admin exists - make the first registered user the admin
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
