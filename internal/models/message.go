package models

import (
	"time"

	"github.com/google/uuid"
)

type Message struct {
	ID               uuid.UUID              `json:"id" db:"id"`
	ChannelID        uuid.UUID              `json:"channelId" db:"channel_id"`
	AuthorID         uuid.UUID              `json:"authorId" db:"author_id"`
	Content          *string                `json:"content,omitempty" db:"content"`
	EncryptedContent []byte                 `json:"-" db:"encrypted_content"`
	ReplyToID        *uuid.UUID             `json:"replyToId,omitempty" db:"reply_to_id"`
	IsEdited         bool                   `json:"isEdited" db:"is_edited"`
	IsPinned         bool                   `json:"isPinned" db:"is_pinned"`
	Reactions        map[string][]uuid.UUID `json:"reactions" db:"reactions"`
	LinkPreviews     []LinkPreview          `json:"linkPreviews,omitempty" db:"link_previews"`
	CreatedAt        time.Time              `json:"createdAt" db:"created_at"`
	UpdatedAt        time.Time              `json:"updatedAt" db:"updated_at"`
	DeletedAt        *time.Time             `json:"-" db:"deleted_at"`
}

type MessageWithAuthor struct {
	Message
	Author      *PublicUser         `json:"author,omitempty"`
	ReplyTo     *Message            `json:"replyTo,omitempty"`
	Attachments []MessageAttachment `json:"attachments,omitempty"`
	Reactions   []ReactionCount     `json:"reactions,omitempty"`
}

type MessageAttachment struct {
	ID           uuid.UUID  `json:"id" db:"id"`
	MessageID    *uuid.UUID `json:"messageId" db:"message_id"`
	UploaderID   uuid.UUID  `json:"uploaderId" db:"uploader_id"`
	Filename     string     `json:"filename" db:"filename"`
	FileURL      string     `json:"url" db:"file_url"`
	FileSize     int64      `json:"size" db:"file_size"`
	ContentType  *string    `json:"contentType,omitempty" db:"content_type"`
	ThumbnailURL *string    `json:"thumbnailUrl,omitempty" db:"thumbnail_url"`
	Width        *int       `json:"width,omitempty" db:"width"`
	Height       *int       `json:"height,omitempty" db:"height"`
	CreatedAt    time.Time  `json:"createdAt" db:"created_at"`
}

type MessageReaction struct {
	ID        uuid.UUID `json:"id" db:"id"`
	MessageID uuid.UUID `json:"messageId" db:"message_id"`
	UserID    uuid.UUID `json:"userId" db:"user_id"`
	Emoji     string    `json:"emoji" db:"emoji"`
	CreatedAt time.Time `json:"createdAt" db:"created_at"`
}

type ReactionCount struct {
	Emoji   string `json:"emoji"`
	Count   int    `json:"count"`
	Reacted bool   `json:"reacted"` // Whether the current user has reacted
}

type LinkPreview struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	SiteName    string `json:"siteName,omitempty"`
	ImageURL    string `json:"imageUrl,omitempty"`
	FaviconURL  string `json:"faviconUrl,omitempty"`
}

// Direct Messages (E2E Encrypted)

type DMConversation struct {
	ID        uuid.UUID `json:"id" db:"id"`
	CreatedAt time.Time `json:"createdAt" db:"created_at"`
	UpdatedAt time.Time `json:"updatedAt" db:"updated_at"`
}

type DMParticipant struct {
	ConversationID uuid.UUID  `json:"conversationId" db:"conversation_id"`
	UserID         uuid.UUID  `json:"userId" db:"user_id"`
	LastReadAt     *time.Time `json:"lastReadAt,omitempty" db:"last_read_at"`
}

type DMConversationWithParticipants struct {
	DMConversation
	Participants []PublicUser   `json:"participants"`
	LastMessage  *DirectMessage `json:"lastMessage,omitempty"`
}

type DirectMessage struct {
	ID               uuid.UUID              `json:"id" db:"id"`
	ConversationID   uuid.UUID              `json:"conversationId" db:"conversation_id"`
	SenderID         uuid.UUID              `json:"senderId" db:"sender_id"`
	EncryptedContent []byte                 `json:"encryptedContent" db:"encrypted_content"`
	Nonce            []byte                 `json:"nonce" db:"nonce"`
	ReplyToID        *uuid.UUID             `json:"replyToId,omitempty" db:"reply_to_id"`
	IsEdited         bool                   `json:"isEdited" db:"is_edited"`
	Reactions        map[string][]uuid.UUID `json:"reactions" db:"reactions"`
	LinkPreviews     []LinkPreview          `json:"linkPreviews,omitempty" db:"link_previews"`
	CreatedAt        time.Time              `json:"createdAt" db:"created_at"`
	UpdatedAt        time.Time              `json:"updatedAt" db:"updated_at"`
	DeletedAt        *time.Time             `json:"-" db:"deleted_at"`
}

type DirectMessageWithSender struct {
	DirectMessage
	Sender *PublicUser `json:"sender,omitempty"`
}
