package runtime

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/tetratelabs/wazero/api"
)

type HostFunctions struct {
	stateStore     *StateStore
	messageService MessageService
	memberService  MemberService
	channelService ChannelService
}

type MessageService interface {
	SendMessageAsPlugin(ctx context.Context, channelID uuid.UUID, content string, pluginID uuid.UUID) error
	DeleteMessageAsPlugin(ctx context.Context, messageID uuid.UUID) error
	AddReactionAsPlugin(ctx context.Context, messageID uuid.UUID, emoji string, pluginID uuid.UUID) error
}

type MemberService interface {
	BanUserAsPlugin(ctx context.Context, communityID, userID uuid.UUID, reason string, pluginID uuid.UUID) error
	KickUserAsPlugin(ctx context.Context, communityID, userID uuid.UUID, pluginID uuid.UUID) error
}

type ChannelService interface {
	GetChannelByID(ctx context.Context, channelID uuid.UUID) (json.RawMessage, error)
}

func NewHostFunctions(stateStore *StateStore, msgSvc MessageService, memberSvc MemberService, channelSvc ChannelService) *HostFunctions {
	return &HostFunctions{
		stateStore:     stateStore,
		messageService: msgSvc,
		memberService:  memberSvc,
		channelService: channelSvc,
	}
}

func (h *HostFunctions) stateGet(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, keyPtr, keyLen uint32) uint64 {
	key := readStringFromMemory(mod, keyPtr, keyLen)
	val, err := h.stateStore.Get(ctx, communityID, pluginID, key)
	if err != nil {
		return 0
	}
	ptr, ln := writeBytesToMemory(mod, val)
	return packPointerLength(ptr, ln)
}

func (h *HostFunctions) stateSet(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, keyPtr, keyLen, valPtr, valLen uint32) int32 {
	key := readStringFromMemory(mod, keyPtr, keyLen)
	val := readRawFromMemory(mod, valPtr, valLen)
	err := h.stateStore.Set(ctx, communityID, pluginID, key, val)
	if err != nil {
		return -1
	}
	return 0
}

func (h *HostFunctions) stateDelete(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, keyPtr, keyLen uint32) int32 {
	key := readStringFromMemory(mod, keyPtr, keyLen)
	err := h.stateStore.Delete(ctx, communityID, pluginID, key)
	if err != nil {
		return -1
	}
	return 0
}

func (h *HostFunctions) stateGetChannel(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, channelIDPtr, channelIDLen, keyPtr, keyLen uint32) uint64 {
	channelIDStr := readStringFromMemory(mod, channelIDPtr, channelIDLen)
	channelID, err := uuid.Parse(channelIDStr)
	if err != nil {
		return 0
	}
	key := readStringFromMemory(mod, keyPtr, keyLen)
	val, err := h.stateStore.GetChannel(ctx, communityID, pluginID, channelID, key)
	if err != nil {
		return 0
	}
	ptr, ln := writeBytesToMemory(mod, val)
	return packPointerLength(ptr, ln)
}

func (h *HostFunctions) stateSetChannel(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, channelIDPtr, channelIDLen, keyPtr, keyLen, valPtr, valLen uint32) int32 {
	channelIDStr := readStringFromMemory(mod, channelIDPtr, channelIDLen)
	channelID, err := uuid.Parse(channelIDStr)
	if err != nil {
		return -1
	}
	key := readStringFromMemory(mod, keyPtr, keyLen)
	val := readRawFromMemory(mod, valPtr, valLen)
	err = h.stateStore.SetChannel(ctx, communityID, pluginID, channelID, key, val)
	if err != nil {
		return -1
	}
	return 0
}

func (h *HostFunctions) actionSendMessage(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, jsonPtr, jsonLen uint32) int32 {
	data := readRawFromMemory(mod, jsonPtr, jsonLen)
	var payload struct {
		ChannelID string `json:"channelId"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return -1
	}
	chID, err := uuid.Parse(payload.ChannelID)
	if err != nil {
		return -1
	}
	if err := h.messageService.SendMessageAsPlugin(ctx, chID, payload.Content, pluginID); err != nil {
		log.Warn().Err(err).Str("plugin", pluginID.String()).Msg("plugin sendMessage failed")
		return -1
	}
	return 0
}

func (h *HostFunctions) actionDeleteMessage(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, jsonPtr, jsonLen uint32) int32 {
	data := readRawFromMemory(mod, jsonPtr, jsonLen)
	var payload struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return -1
	}
	msgID, err := uuid.Parse(payload.MessageID)
	if err != nil {
		return -1
	}
	if err := h.messageService.DeleteMessageAsPlugin(ctx, msgID); err != nil {
		return -1
	}
	return 0
}

func (h *HostFunctions) actionBanUser(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, jsonPtr, jsonLen uint32) int32 {
	data := readRawFromMemory(mod, jsonPtr, jsonLen)
	var payload struct {
		UserID string `json:"userId"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return -1
	}
	userID, err := uuid.Parse(payload.UserID)
	if err != nil {
		return -1
	}
	if err := h.memberService.BanUserAsPlugin(ctx, communityID, userID, payload.Reason, pluginID); err != nil {
		return -1
	}
	return 0
}

func (h *HostFunctions) actionKickUser(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, jsonPtr, jsonLen uint32) int32 {
	data := readRawFromMemory(mod, jsonPtr, jsonLen)
	var payload struct {
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return -1
	}
	userID, err := uuid.Parse(payload.UserID)
	if err != nil {
		return -1
	}
	if err := h.memberService.KickUserAsPlugin(ctx, communityID, userID, pluginID); err != nil {
		return -1
	}
	return 0
}

func (h *HostFunctions) actionAddReaction(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, jsonPtr, jsonLen uint32) int32 {
	data := readRawFromMemory(mod, jsonPtr, jsonLen)
	var payload struct {
		MessageID string `json:"messageId"`
		Emoji     string `json:"emoji"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return -1
	}
	msgID, err := uuid.Parse(payload.MessageID)
	if err != nil {
		return -1
	}
	if err := h.messageService.AddReactionAsPlugin(ctx, msgID, payload.Emoji, pluginID); err != nil {
		return -1
	}
	return 0
}

func (h *HostFunctions) queryGetMember(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, jsonPtr, jsonLen uint32) uint64 {
	return 0
}

func (h *HostFunctions) queryGetChannel(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, jsonPtr, jsonLen uint32) uint64 {
	return 0
}

func (h *HostFunctions) queryGetMessages(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID, jsonPtr, jsonLen uint32) uint64 {
	return 0
}

func (h *HostFunctions) log(ctx context.Context, mod api.Module, levelPtr, levelLen, msgPtr, msgLen uint32) int32 {
	level := readStringFromMemory(mod, levelPtr, levelLen)
	msg := readStringFromMemory(mod, msgPtr, msgLen)

	switch level {
	case "error":
		log.Error().Msg(msg)
	case "warn":
		log.Warn().Msg(msg)
	case "debug":
		log.Debug().Msg(msg)
	default:
		log.Info().Msg(msg)
	}
	return 0
}

func (h *HostFunctions) getConfig(ctx context.Context, mod api.Module, pluginID, communityID uuid.UUID) uint64 {
	return 0
}

func (h *HostFunctions) getPluginID(ctx context.Context, mod api.Module, pluginID uuid.UUID) uint64 {
	bytes := []byte(pluginID.String())
	ptr, ln := writeBytesToMemory(mod, bytes)
	return packPointerLength(ptr, ln)
}

func (h *HostFunctions) getCommunityID(ctx context.Context, mod api.Module, communityID uuid.UUID) uint64 {
	bytes := []byte(communityID.String())
	ptr, ln := writeBytesToMemory(mod, bytes)
	return packPointerLength(ptr, ln)
}
