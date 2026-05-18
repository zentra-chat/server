package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
)

func (s *Service) GetDashboard(ctx context.Context) (*DashboardStats, error) {
	stats := &DashboardStats{}

	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL`).Scan(&stats.TotalUsers)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total users")
		return nil, err
	}

	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages`).Scan(&stats.TotalMessages)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total messages")
		return nil, err
	}

	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM communities WHERE deleted_at IS NULL`).Scan(&stats.TotalCommunities)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total communities")
		return nil, err
	}

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

	return stats, nil
}

func (s *Service) GetAnalytics(ctx context.Context) (*AnalyticsStats, error) {
	stats := &AnalyticsStats{}

	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL`).Scan(&stats.TotalUsers); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total users")
	}
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages`).Scan(&stats.TotalMessages); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total messages")
	}
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM communities WHERE deleted_at IS NULL`).Scan(&stats.TotalCommunities); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total communities")
	}
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM channels`).Scan(&stats.TotalChannels); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch total channels")
	}

	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE status = 'online' AND deleted_at IS NULL`).Scan(&stats.OnlineUsers); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch online users")
	}

	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= CURRENT_DATE`).Scan(&stats.MessagesToday); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch messages today")
	}

	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= CURRENT_DATE`).Scan(&stats.NewUsersToday); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch new users today")
	}

	if err := s.db.QueryRow(ctx, `SELECT COUNT(DISTINCT author_id) FROM messages WHERE created_at >= NOW() - INTERVAL '7 days'`).Scan(&stats.ActiveUsers7d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch active users 7d")
	}

	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '7 days'`).Scan(&stats.NewUsers7d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch new users 7d")
	}

	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND created_at >= NOW() - INTERVAL '30 days'`).Scan(&stats.NewUsers30d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch new users 30d")
	}

	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= NOW() - INTERVAL '7 days'`).Scan(&stats.Messages7d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch messages 7d")
	}

	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE created_at >= NOW() - INTERVAL '30 days'`).Scan(&stats.Messages30d); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch messages 30d")
	}

	if stats.TotalUsers > 0 {
		stats.AvgMessagesPerUser = float64(stats.TotalMessages) / float64(stats.TotalUsers)
	}

	if err := s.db.QueryRow(ctx, `SELECT COALESCE(AVG(member_count), 0) FROM communities WHERE deleted_at IS NULL AND member_count > 0`).Scan(&stats.AvgMembersPerCommunity); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch avg members per community")
	}

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

	stats.ActiveUsersOverTime, err = s.getCountOverTime(ctx,
		`SELECT DATE(created_at)::text, COUNT(DISTINCT author_id) FROM messages WHERE created_at >= NOW() - INTERVAL '30 days' GROUP BY DATE(created_at) ORDER BY DATE(created_at)`,
	)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch active users over time")
	}

	stats.TopCommunities = s.getTopCommunities(ctx)
	stats.ActiveHours = s.getActiveHours(ctx)
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
