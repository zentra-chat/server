package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type ChannelType string

// Built-in channel types. Plugins can add more at runtime.
const (
	ChannelTypeText         ChannelType = "text"
	ChannelTypeAnnouncement ChannelType = "announcement"
	ChannelTypeGallery      ChannelType = "gallery"
	ChannelTypeForum        ChannelType = "forum"
	ChannelTypeVoice        ChannelType = "voice"
)

// Capability flags for channel types - determines what features a channel supports
const (
	CapMessages  int64 = 1 << iota // 1 - basic text messaging
	CapThreads                     // 2 - threaded replies
	CapMedia                       // 4 - media-first content
	CapVoice                       // 8 - real-time voice
	CapVideo                       // 16 - real-time video
	CapEmbeds                      // 32 - rich embeds and link previews
	CapPins                        // 64 - message pinning
	CapReactions                   // 128 - emoji reactions
	CapSlowmode                    // 256 - rate limiting per user
	CapReadOnly                    // 512 - only privileged users can post
	CapTopics                      // 1024 - topic/thread-starter based
)

// ChannelTypeDefinition describes a registered channel type
type ChannelTypeDefinition struct {
	ID              string          `json:"id" db:"id"`
	Name            string          `json:"name" db:"name"`
	Description     string          `json:"description" db:"description"`
	Icon            string          `json:"icon" db:"icon"`
	Capabilities    int64           `json:"capabilities" db:"capabilities"`
	DefaultMetadata json.RawMessage `json:"defaultMetadata" db:"default_metadata"`
	BuiltIn         bool            `json:"builtIn" db:"built_in"`
	PluginID        *string         `json:"pluginId,omitempty" db:"plugin_id"`
	CreatedAt       time.Time       `json:"createdAt" db:"created_at"`
}

// HasCapability checks whether a type definition supports a given capability
func (d *ChannelTypeDefinition) HasCapability(cap int64) bool {
	return d.Capabilities&cap != 0
}

// VoiceState represents a user's voice connection state in a voice channel
type VoiceState struct {
	ID              uuid.UUID `json:"id" db:"id"`
	ChannelID       uuid.UUID `json:"channelId" db:"channel_id"`
	UserID          uuid.UUID `json:"userId" db:"user_id"`
	IsMuted         bool      `json:"isMuted" db:"is_muted"`
	IsDeafened      bool      `json:"isDeafened" db:"is_deafened"`
	IsSelfMuted     bool      `json:"isSelfMuted" db:"is_self_muted"`
	IsSelfDeaf      bool      `json:"isSelfDeafened" db:"is_self_deafened"`
	IsScreenSharing bool      `json:"isScreenSharing" db:"is_screen_sharing"`
	JoinedAt        time.Time `json:"joinedAt" db:"joined_at"`
}

// VoiceStateWithUser includes user info for display
type VoiceStateWithUser struct {
	VoiceState
	User *User `json:"user,omitempty"`
}

type ChannelCategory struct {
	ID          uuid.UUID `json:"id" db:"id"`
	CommunityID uuid.UUID `json:"communityId" db:"community_id"`
	Name        string    `json:"name" db:"name"`
	Position    int       `json:"position" db:"position"`
	CreatedAt   time.Time `json:"createdAt" db:"created_at"`
}

type Channel struct {
	ID              uuid.UUID       `json:"id" db:"id"`
	CommunityID     uuid.UUID       `json:"communityId" db:"community_id"`
	CategoryID      *uuid.UUID      `json:"categoryId,omitempty" db:"category_id"`
	Name            string          `json:"name" db:"name"`
	Topic           *string         `json:"topic,omitempty" db:"topic"`
	Type            ChannelType     `json:"type" db:"type"`
	Position        int             `json:"position" db:"position"`
	IsNSFW          bool            `json:"isNsfw" db:"is_nsfw"`
	SlowmodeSeconds int             `json:"slowmodeSeconds" db:"slowmode_seconds"`
	Metadata        json.RawMessage `json:"metadata" db:"metadata"`
	LastMessageAt   *time.Time      `json:"lastMessageAt,omitempty" db:"last_message_at"`
	CreatedAt       time.Time       `json:"createdAt" db:"created_at"`
	UpdatedAt       time.Time       `json:"updatedAt" db:"updated_at"`
}

type ChannelPermission struct {
	ID               uuid.UUID `json:"id" db:"id"`
	ChannelID        uuid.UUID `json:"channelId" db:"channel_id"`
	TargetType       string    `json:"targetType" db:"target_type"` // "role" or "member"
	TargetID         uuid.UUID `json:"targetId" db:"target_id"`
	AllowPermissions int64     `json:"allowPermissions" db:"allow_permissions"`
	DenyPermissions  int64     `json:"denyPermissions" db:"deny_permissions"`
}

type ChannelWithCategory struct {
	Channel
	CategoryName *string `json:"categoryName,omitempty" db:"category_name"`
}

type ChannelReadState struct {
	UserID      uuid.UUID `json:"userId" db:"user_id"`
	ChannelID   uuid.UUID `json:"channelId" db:"channel_id"`
	LastReadAt  time.Time `json:"lastReadAt" db:"last_read_at"`
}

type UnreadCount struct {
	ChannelID uuid.UUID `json:"channelId" db:"channel_id"`
	Count     int       `json:"count" db:"unread_count"`
}
