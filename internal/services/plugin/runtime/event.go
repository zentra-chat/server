package runtime

import "encoding/json"

type Action struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

const (
	ActionBlockMessage  = "block_message"
	ActionDeleteMessage = "delete_message"
	ActionSendMessage   = "send_message"
	ActionBanUser       = "ban_user"
	ActionKickUser      = "kick_user"
	ActionAddReaction   = "add_reaction"
)

type ActionList struct {
	Actions []Action `json:"actions"`
}

type MessageCreateEvent struct {
	MessageID   string `json:"messageId"`
	ChannelID   string `json:"channelId"`
	AuthorID    string `json:"authorId"`
	Content     string `json:"content"`
	CommunityID string `json:"communityId"`
}

type MessageUpdateEvent struct {
	MessageID   string `json:"messageId"`
	ChannelID   string `json:"channelId"`
	AuthorID    string `json:"authorId"`
	Content     string `json:"content"`
	CommunityID string `json:"communityId"`
}

type MessageDeleteEvent struct {
	MessageID   string `json:"messageId"`
	ChannelID   string `json:"channelId"`
	CommunityID string `json:"communityId"`
}

type MemberJoinEvent struct {
	UserID      string `json:"userId"`
	CommunityID string `json:"communityId"`
}

type MemberLeaveEvent struct {
	UserID      string `json:"userId"`
	CommunityID string `json:"communityId"`
}

type ReactionAddEvent struct {
	MessageID   string `json:"messageId"`
	ChannelID   string `json:"channelId"`
	UserID      string `json:"userId"`
	Emoji       string `json:"emoji"`
	CommunityID string `json:"communityId"`
}

type ReactionRemoveEvent struct {
	MessageID   string `json:"messageId"`
	ChannelID   string `json:"channelId"`
	UserID      string `json:"userId"`
	Emoji       string `json:"emoji"`
	CommunityID string `json:"communityId"`
}

type ChannelCreateEvent struct {
	ChannelID   string `json:"channelId"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	CommunityID string `json:"communityId"`
}

type ChannelDeleteEvent struct {
	ChannelID   string `json:"channelId"`
	CommunityID string `json:"communityId"`
}

type SendMessagePayload struct {
	ChannelID string `json:"channelId"`
	Content   string `json:"content"`
}

type DeleteMessagePayload struct {
	MessageID string `json:"messageId"`
}

type BanUserPayload struct {
	UserID string `json:"userId"`
	Reason string `json:"reason"`
}

type KickUserPayload struct {
	UserID string `json:"userId"`
}

type AddReactionPayload struct {
	MessageID string `json:"messageId"`
	Emoji     string `json:"emoji"`
}
