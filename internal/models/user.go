package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type UserStatus string

const (
	UserStatusOnline    UserStatus = "online"
	UserStatusAway      UserStatus = "away"
	UserStatusBusy      UserStatus = "busy"
	UserStatusInvisible UserStatus = "invisible"
	UserStatusOffline   UserStatus = "offline"
)

type User struct {
	ID               uuid.UUID  `json:"id" db:"id"`
	Username         string     `json:"username" db:"username"`
	Email            string     `json:"email,omitempty" db:"email"`
	PasswordHash     string     `json:"-" db:"password_hash"`
	DisplayName      *string    `json:"displayName,omitempty" db:"display_name"`
	AvatarURL        *string    `json:"avatarUrl,omitempty" db:"avatar_url"`
	BannerURL        *string    `json:"bannerUrl,omitempty" db:"banner_url"`
	Bio              *string    `json:"bio,omitempty" db:"bio"`
	Status           UserStatus `json:"status" db:"status"`
	CustomStatus     *string    `json:"customStatus,omitempty" db:"custom_status"`
	EmailVerified    bool       `json:"emailVerified" db:"email_verified"`
	TwoFactorEnabled bool       `json:"twoFactorEnabled" db:"two_factor_enabled"`
	TwoFactorSecret  *string    `json:"-" db:"two_factor_secret"`
	CreatedAt        time.Time  `json:"createdAt" db:"created_at"`
	UpdatedAt        time.Time  `json:"updatedAt" db:"updated_at"`
	LastSeenAt       *time.Time `json:"lastSeenAt,omitempty" db:"last_seen_at"`
	IsAdmin          bool              `json:"isAdmin" db:"is_admin"`
	CommunityLayout  *json.RawMessage  `json:"communityLayout,omitempty" db:"community_layout"`
	DeletedAt        *time.Time        `json:"-" db:"deleted_at"`
}

type UserSession struct {
	ID               uuid.UUID  `json:"id" db:"id"`
	UserID           uuid.UUID  `json:"userId" db:"user_id"`
	RefreshTokenHash string     `json:"-" db:"refresh_token_hash"`
	DeviceInfo       *string    `json:"deviceInfo,omitempty" db:"device_info"`
	IPAddress        *string    `json:"ipAddress,omitempty" db:"ip_address"`
	ExpiresAt        time.Time  `json:"expiresAt" db:"expires_at"`
	CreatedAt        time.Time  `json:"createdAt" db:"created_at"`
	RevokedAt        *time.Time `json:"-" db:"revoked_at"`
}

type UserSettings struct {
	UserID               uuid.UUID       `json:"userId" db:"user_id"`
	Theme                string          `json:"theme" db:"theme"`
	NotificationsEnabled bool            `json:"notificationsEnabled" db:"notifications_enabled"`
	SoundEnabled         bool            `json:"soundEnabled" db:"sound_enabled"`
	CompactMode          bool            `json:"compactMode" db:"compact_mode"`
	SettingsJSON         json.RawMessage `json:"settings" db:"settings_json"`
	UpdatedAt            time.Time       `json:"updatedAt" db:"updated_at"`
}

type UserBlock struct {
	BlockerID uuid.UUID `json:"blockerId" db:"blocker_id"`
	BlockedID uuid.UUID `json:"blockedId" db:"blocked_id"`
	CreatedAt time.Time `json:"createdAt" db:"created_at"`
}

type FriendRequest struct {
	User      *PublicUser `json:"user"`
	CreatedAt time.Time   `json:"createdAt"`
}

type FriendRequests struct {
	Incoming []*FriendRequest `json:"incoming"`
	Outgoing []*FriendRequest `json:"outgoing"`
}

type UserRelationshipStatus string

const (
	UserRelationshipNone            UserRelationshipStatus = "none"
	UserRelationshipFriends         UserRelationshipStatus = "friends"
	UserRelationshipIncomingRequest UserRelationshipStatus = "incoming_request"
	UserRelationshipOutgoingRequest UserRelationshipStatus = "outgoing_request"
	UserRelationshipBlocked         UserRelationshipStatus = "blocked"
	UserRelationshipBlockedBy       UserRelationshipStatus = "blocked_by"
)

type UserRelationship struct {
	UserID uuid.UUID              `json:"userId"`
	Status UserRelationshipStatus `json:"status"`
}

// PublicUser is a sanitized user for public API responses
type PublicUser struct {
	ID           uuid.UUID  `json:"id"`
	Username     string     `json:"username"`
	DisplayName  *string    `json:"displayName,omitempty"`
	AvatarURL    *string    `json:"avatarUrl,omitempty"`
	BannerURL    *string    `json:"bannerUrl,omitempty"`
	Bio          *string    `json:"bio,omitempty"`
	Status       UserStatus `json:"status"`
	CustomStatus *string    `json:"customStatus,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
}

func (u *User) ToPublic() *PublicUser {
	return &PublicUser{
		ID:           u.ID,
		Username:     u.Username,
		DisplayName:  u.DisplayName,
		AvatarURL:    u.AvatarURL,
		BannerURL:    u.BannerURL,
		Bio:          u.Bio,
		Status:       u.Status,
		CustomStatus: u.CustomStatus,
		CreatedAt:    u.CreatedAt,
	}
}
