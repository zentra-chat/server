package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func packPointerLength(ptr, length uint32) uint64 {
	return uint64(ptr)<<32 | uint64(length)
}

func unpackPointerLength(packed uint64) (uint32, uint32) {
	return uint32(packed >> 32), uint32(packed & math.MaxUint32)
}

func readStringFromMemory(mod api.Module, ptr, length uint32) string {
	bytes, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return ""
	}
	return string(bytes)
}

func readRawFromMemory(mod api.Module, ptr, length uint32) json.RawMessage {
	bytes, ok := mod.Memory().Read(ptr, length)
	if !ok {
		return nil
	}
	return json.RawMessage(bytes)
}

func writeBytesToMemory(mod api.Module, data []byte) (uint32, uint32) {
	alloc := mod.ExportedFunction("alloc")
	if alloc == nil {
		return 0, 0
	}
	results, err := alloc.Call(context.Background(), uint64(len(data)))
	if err != nil {
		return 0, 0
	}
	ptr := uint32(results[0])
	if !mod.Memory().Write(ptr, data) {
		return 0, 0
	}
	return ptr, uint32(len(data))
}

func readActionListFromMemory(mod api.Module, ptr uint32) ([]Action, error) {
	memLen := mod.Memory().Size()
	lenVal := memLen - ptr
	if lenVal > 65536 {
		lenVal = 65536
	}
	outBytes, ok := mod.Memory().Read(ptr, lenVal)
	if !ok {
		return nil, fmt.Errorf("failed to read action list from wasm memory")
	}

	nulIdx := 0
	for nulIdx < len(outBytes) && outBytes[nulIdx] != 0 {
		nulIdx++
	}
	outBytes = outBytes[:nulIdx]

	var actionList ActionList
	if err := json.Unmarshal(outBytes, &actionList); err != nil {
		return nil, fmt.Errorf("parse action list: %w", err)
	}

	return actionList.Actions, nil
}

func buildHostModule(ctx context.Context, wasmRuntime wazero.Runtime, host *HostFunctions, pluginID, communityID uuid.UUID) {
	hb := wasmRuntime.NewHostModuleBuilder("env")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, keyPtr, keyLen uint32) uint64 {
		return host.stateGet(ctx, m, pluginID, communityID, keyPtr, keyLen)
	}).Export("state_get")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, keyPtr, keyLen, valPtr, valLen uint32) int32 {
		return host.stateSet(ctx, m, pluginID, communityID, keyPtr, keyLen, valPtr, valLen)
	}).Export("state_set")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, keyPtr, keyLen uint32) int32 {
		return host.stateDelete(ctx, m, pluginID, communityID, keyPtr, keyLen)
	}).Export("state_delete")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, chPtr, chLen, keyPtr, keyLen uint32) uint64 {
		return host.stateGetChannel(ctx, m, pluginID, communityID, chPtr, chLen, keyPtr, keyLen)
	}).Export("state_get_channel")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, chPtr, chLen, keyPtr, keyLen, valPtr, valLen uint32) int32 {
		return host.stateSetChannel(ctx, m, pluginID, communityID, chPtr, chLen, keyPtr, keyLen, valPtr, valLen)
	}).Export("state_set_channel")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, jsonPtr, jsonLen uint32) int32 {
		return host.actionSendMessage(ctx, m, pluginID, communityID, jsonPtr, jsonLen)
	}).Export("action_send_message")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, jsonPtr, jsonLen uint32) int32 {
		return host.actionDeleteMessage(ctx, m, pluginID, communityID, jsonPtr, jsonLen)
	}).Export("action_delete_message")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, jsonPtr, jsonLen uint32) int32 {
		return host.actionBanUser(ctx, m, pluginID, communityID, jsonPtr, jsonLen)
	}).Export("action_ban_user")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, jsonPtr, jsonLen uint32) int32 {
		return host.actionKickUser(ctx, m, pluginID, communityID, jsonPtr, jsonLen)
	}).Export("action_kick_user")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, jsonPtr, jsonLen uint32) int32 {
		return host.actionAddReaction(ctx, m, pluginID, communityID, jsonPtr, jsonLen)
	}).Export("action_add_reaction")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, jsonPtr, jsonLen uint32) uint64 {
		return host.queryGetMember(ctx, m, pluginID, communityID, jsonPtr, jsonLen)
	}).Export("query_get_member")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, jsonPtr, jsonLen uint32) uint64 {
		return host.queryGetChannel(ctx, m, pluginID, communityID, jsonPtr, jsonLen)
	}).Export("query_get_channel")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, jsonPtr, jsonLen uint32) uint64 {
		return host.queryGetMessages(ctx, m, pluginID, communityID, jsonPtr, jsonLen)
	}).Export("query_get_messages")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, levelPtr, levelLen, msgPtr, msgLen uint32) int32 {
		return host.log(ctx, m, levelPtr, levelLen, msgPtr, msgLen)
	}).Export("log")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module) uint64 {
		return host.getConfig(ctx, m, pluginID, communityID)
	}).Export("get_config")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module) uint64 {
		return host.getPluginID(ctx, m, pluginID)
	}).Export("get_plugin_id")

	hb.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module) uint64 {
		return host.getCommunityID(ctx, m, communityID)
	}).Export("get_community_id")

	_, _ = hb.Instantiate(ctx)
}
