package models

import (
	"time"

	"github.com/google/uuid"
)

// NotificationType describes what triggered a notification.
type NotificationType string

const (
	// Mention-based notifications
	NotificationTypeMentionUser     NotificationType = "mention_user"
	NotificationTypeMentionRole     NotificationType = "mention_role"
	NotificationTypeMentionEveryone NotificationType = "mention_everyone"
	NotificationTypeMentionHere     NotificationType = "mention_here"

	// Interaction notifications
	NotificationTypeReply     NotificationType = "reply"
	NotificationTypeDMMessage NotificationType = "dm_message"
)

// MentionType describes the kind of mention encoded in a message.
type MentionType string

const (
	MentionTypeUser     MentionType = "user"
	MentionTypeRole     MentionType = "role"
	MentionTypeEveryone MentionType = "everyone"
	MentionTypeHere     MentionType = "here"
)

// Notification represents a single notification for a user.
type Notification struct {
	ID          uuid.UUID        `json:"id" db:"id"`
	UserID      uuid.UUID        `json:"userId" db:"user_id"`
	Type        NotificationType `json:"type" db:"type"`
	Title       string           `json:"title" db:"title"`
	Body        *string          `json:"body,omitempty" db:"body"`
	CommunityID *uuid.UUID       `json:"communityId,omitempty" db:"community_id"`
	ChannelID   *uuid.UUID       `json:"channelId,omitempty" db:"channel_id"`
	MessageID   *uuid.UUID       `json:"messageId,omitempty" db:"message_id"`
	ActorID     *uuid.UUID       `json:"actorId,omitempty" db:"actor_id"`
	Metadata    map[string]any   `json:"metadata,omitempty" db:"metadata"`
	IsRead      bool             `json:"isRead" db:"is_read"`
	CreatedAt   time.Time        `json:"createdAt" db:"created_at"`

	// Joined fields (populated on read, not stored in DB column)
	Actor *PublicUser `json:"actor,omitempty"`
}

// MessageMention represents a mention record stored for a message.
type MessageMention struct {
	ID              uuid.UUID   `json:"id" db:"id"`
	MessageID       uuid.UUID   `json:"messageId" db:"message_id"`
	ChannelID       uuid.UUID   `json:"channelId" db:"channel_id"`
	CommunityID     *uuid.UUID  `json:"communityId,omitempty" db:"community_id"`
	AuthorID        uuid.UUID   `json:"authorId" db:"author_id"`
	MentionedUserID *uuid.UUID  `json:"mentionedUserId,omitempty" db:"mentioned_user_id"`
	MentionedRoleID *uuid.UUID  `json:"mentionedRoleId,omitempty" db:"mentioned_role_id"`
	MentionType     MentionType `json:"mentionType" db:"mention_type"`
	CreatedAt       time.Time   `json:"createdAt" db:"created_at"`
}
