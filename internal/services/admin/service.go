package admin

import (
	"context"
	"errors"
	"fmt"
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

func (s *Service) GetAnalytics(ctx context.Context) (*AnalyticsStats, error) {
	stats := &AnalyticsStats{}

	// Total users
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL`).Scan(&stats.TotalUsers)
	// Total messages
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages`).Scan(&stats.TotalMessages)
	// Total communities
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM communities WHERE deleted_at IS NULL`).Scan(&stats.TotalCommunities)
	// Total channels
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM channels`).Scan(&stats.TotalChannels)

	// Online users
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE status = 'online' AND deleted_at IS NULL`).Scan(&stats.OnlineUsers)

	// Messages today
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= CURRENT_DATE`).Scan(&stats.MessagesToday)

	// New users today
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= CURRENT_DATE`).Scan(&stats.NewUsersToday)

	// Active users in the last 7 days (users who sent a message)
	_ = s.db.QueryRow(ctx, `SELECT COUNT(DISTINCT author_id) FROM messages WHERE created_at >= NOW() - INTERVAL '7 days'`).Scan(&stats.ActiveUsers7d)

	// New users in the last 7 days
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '7 days'`).Scan(&stats.NewUsers7d)

	// New users in the last 30 days
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '30 days'`).Scan(&stats.NewUsers30d)

	// Messages in the last 7 days
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= NOW() - INTERVAL '7 days'`).Scan(&stats.Messages7d)

	// Messages in the last 30 days
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= NOW() - INTERVAL '30 days'`).Scan(&stats.Messages30d)

	// Average messages per user
	if stats.TotalUsers > 0 {
		stats.AvgMessagesPerUser = float64(stats.TotalMessages) / float64(stats.TotalUsers)
	}

	// Average members per community (only communities with members)
	_ = s.db.QueryRow(ctx, `SELECT COALESCE(AVG(member_count), 0) FROM communities WHERE deleted_at IS NULL AND member_count > 0`).Scan(&stats.AvgMembersPerCommunity)

	// Growth rates (compare last 7 days to previous 7 days)
	var prev7Users, prev7Messages, prev7Communities int64
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '14 days' AND created_at < NOW() - INTERVAL '7 days'`).Scan(&prev7Users)
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= NOW() - INTERVAL '14 days' AND created_at < NOW() - INTERVAL '7 days'`).Scan(&prev7Messages)
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM communities WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '14 days' AND created_at < NOW() - INTERVAL '7 days'`).Scan(&prev7Communities)

	if prev7Users > 0 {
		stats.UserGrowthRate = (float64(stats.NewUsers7d) - float64(prev7Users)) / float64(prev7Users) * 100
	}
	if prev7Messages > 0 {
		stats.MessageGrowthRate = (float64(stats.Messages7d) - float64(prev7Messages)) / float64(prev7Messages) * 100
	}
	var newCommunities7d int64
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM communities WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '7 days'`).Scan(&newCommunities7d)
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
			continue
		}
		hourMap[h] = c
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
			continue
		}
		dayMap[d] = c
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
