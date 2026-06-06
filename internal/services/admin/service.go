package admin

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/zentra/server/config"
)

var Version = "dev"

var (
	ErrNotAdmin          = errors.New("user is not an admin")
	ErrAlreadyAdmin      = errors.New("user is already an admin")
	ErrCannotRemoveSelf  = errors.New("cannot remove yourself as admin")
	ErrUserNotFound      = errors.New("user not found")
	ErrLastAdmin         = errors.New("cannot remove the last admin")
	ErrUserNotDeleted    = errors.New("user is not deleted")
	ErrUsernameTaken     = errors.New("username is already taken")
	ErrEmailTaken        = errors.New("email is already taken")
	ErrCannotDeleteSelf  = errors.New("cannot delete yourself")
	ErrCannotDeleteAdmin = errors.New("cannot delete an admin user")
)

type Service struct {
	db      *pgxpool.Pool
	rdb     *redis.Client
	cfg     *config.Config
	startAt time.Time

	backendDir   string
	frontendDir  string
	updateMethod string
	updateCommand string
	updateMu     sync.Mutex
	updateTasks  map[string]*updateTask
}

func NewService(db *pgxpool.Pool, rdb *redis.Client, cfg *config.Config) *Service {
	backendDir := os.Getenv("BACKEND_DIR")
	if backendDir == "" {
		backendDir = "."
	}

	frontendDir := os.Getenv("FRONTEND_DIR")

	updateMethod := os.Getenv("UPDATE_METHOD")
	if updateMethod == "" {
		updateMethod = "docker"
	}

	return &Service{
		db:            db,
		rdb:           rdb,
		cfg:           cfg,
		startAt:       time.Now(),
		backendDir:    backendDir,
		frontendDir:   frontendDir,
		updateMethod:  updateMethod,
		updateCommand: os.Getenv("UPDATE_COMMAND"),
		updateTasks:   make(map[string]*updateTask),
	}
}

// DataPoint represents a metric measurement at a point in time
type DataPoint struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}

// DashboardStats holds the aggregated instance statistics
type DashboardStats struct {
	TotalUsers       int64       `json:"totalUsers"`
	TotalMessages    int64       `json:"totalMessages"`
	TotalCommunities int64       `json:"totalCommunities"`
	UsersOverTime    []DataPoint `json:"usersOverTime"`
	MessagesOverTime []DataPoint `json:"messagesOverTime"`
	CommunitiesOverTime []DataPoint `json:"communitiesOverTime"`
}

// AnalyticsStats holds the comprehensive instance analytics
type AnalyticsStats struct {
	TotalUsers              int64           `json:"totalUsers"`
	TotalMessages           int64           `json:"totalMessages"`
	TotalCommunities        int64           `json:"totalCommunities"`
	TotalChannels           int64           `json:"totalChannels"`
	OnlineUsers             int64           `json:"onlineUsers"`
	MessagesToday           int64           `json:"messagesToday"`
	NewUsersToday           int64           `json:"newUsersToday"`
	ActiveUsers7d           int64           `json:"activeUsers7d"`
	NewUsers7d              int64           `json:"newUsers7d"`
	NewUsers30d             int64           `json:"newUsers30d"`
	Messages7d              int64           `json:"messages7d"`
	Messages30d             int64           `json:"messages30d"`
	AvgMessagesPerUser      float64         `json:"avgMessagesPerUser"`
	AvgMembersPerCommunity  float64         `json:"avgMembersPerCommunity"`
	UserGrowthRate          float64         `json:"userGrowthRate"`
	MessageGrowthRate       float64         `json:"messageGrowthRate"`
	CommunityGrowthRate     float64         `json:"communityGrowthRate"`
	UsersOverTime           []DataPoint     `json:"usersOverTime"`
	MessagesOverTime        []DataPoint     `json:"messagesOverTime"`
	CommunitiesOverTime     []DataPoint     `json:"communitiesOverTime"`
	ActiveUsersOverTime     []DataPoint     `json:"activeUsersOverTime"`
	TopCommunities          []CommunityStat `json:"topCommunities"`
	ActiveHours             []HourlyStat    `json:"activeHours"`
	ActiveWeekdays          []DailyStat     `json:"activeWeekdays"`
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
