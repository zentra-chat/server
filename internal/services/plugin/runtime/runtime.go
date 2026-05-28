package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"github.com/tetratelabs/wazero"
)

type cachedModule struct {
	compiled wazero.CompiledModule
	binary   []byte
}

type Runtime struct {
	mu           sync.RWMutex
	modules      map[string]*cachedModule
	pool         *InstancePool
	store        *StateStore
	host         *HostFunctions
	subIndex     *SubscriptionIndex
	db           *pgxpool.Pool
}

func NewRuntime(db *pgxpool.Pool, redis *redis.Client, msgSvc MessageService, memberSvc MemberService, channelSvc ChannelService) *Runtime {
	store := NewStateStore(db, redis)
	return &Runtime{
		modules:  make(map[string]*cachedModule),
		pool:     NewInstancePool(store),
		store:    store,
		host:     NewHostFunctions(store, msgSvc, memberSvc, channelSvc),
		subIndex: NewSubscriptionIndex(),
		db:       db,
	}
}

type SubscriptionIndex struct {
	mu         sync.RWMutex
	community  map[string]map[string]struct{}
}

func NewSubscriptionIndex() *SubscriptionIndex {
	return &SubscriptionIndex{
		community: make(map[string]map[string]struct{}),
	}
}

func (idx *SubscriptionIndex) Subscribe(communityID uuid.UUID, pluginID uuid.UUID) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	cid := communityID.String()
	if idx.community[cid] == nil {
		idx.community[cid] = make(map[string]struct{})
	}
	idx.community[cid][pluginID.String()] = struct{}{}
}

func (idx *SubscriptionIndex) Unsubscribe(communityID uuid.UUID, pluginID uuid.UUID) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	cid := communityID.String()
	if m, ok := idx.community[cid]; ok {
		delete(m, pluginID.String())
		if len(m) == 0 {
			delete(idx.community, cid)
		}
	}
}

func (idx *SubscriptionIndex) HasSubscribers(communityID uuid.UUID) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	_, ok := idx.community[communityID.String()]
	return ok
}

func (idx *SubscriptionIndex) GetPlugins(communityID uuid.UUID) []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	m := idx.community[communityID.String()]
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	return out
}

func (r *Runtime) LoadModule(ctx context.Context, pluginID uuid.UUID, wasmBytes []byte) error {
	wasmRuntime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig())
	defer wasmRuntime.Close(ctx)

	compiled, err := wasmRuntime.CompileModule(ctx, wasmBytes)
	if err != nil {
		return fmt.Errorf("compile wasm module: %w", err)
	}

	// Store the binary alongside the compiled module since
	// wazero.CompiledModule does not expose a Binary() method
	binaryCopy := make([]byte, len(wasmBytes))
	copy(binaryCopy, wasmBytes)

	r.mu.Lock()
	r.modules[pluginID.String()] = &cachedModule{compiled: compiled, binary: binaryCopy}
	r.mu.Unlock()

	return nil
}

func (r *Runtime) Dispatch(ctx context.Context, pluginID, communityID uuid.UUID, eventType string, data json.RawMessage) ([]Action, error) {
	if !r.subIndex.HasSubscribers(communityID) {
		return nil, nil
	}

	r.mu.RLock()
	cached, ok := r.modules[pluginID.String()]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("module not loaded for plugin %s", pluginID)
	}

	wasmBytes := cached.binary

	inst, err := r.pool.Get(ctx, pluginID, communityID, wasmBytes, r.host)
	if err != nil {
		if err.Error() == fmt.Sprintf("circuit breaker open for plugin %s in community %s", pluginID, communityID) {
			return nil, nil
		}
		return nil, fmt.Errorf("get instance: %w", err)
	}

	var actions []Action
	if eventType == "" {
		return nil, nil
	}

	if eventType == "command" {
		return nil, nil
	}

	actions, err = inst.HandleEvent(ctx, eventType, data)
	if err != nil {
		log.Warn().Err(err).Str("plugin", pluginID.String()).Str("event", eventType).Msg("plugin event handler error")
	}

	r.pool.Put(inst)

	return actions, nil
}

func (r *Runtime) DispatchCommand(ctx context.Context, pluginID, communityID uuid.UUID, command string, args json.RawMessage) ([]Action, error) {
	return r.Dispatch(ctx, pluginID, communityID, "command:"+command, args)
}

func (r *Runtime) StateStore() *StateStore {
	return r.store
}

func (r *Runtime) SubscriptionIndex() *SubscriptionIndex {
	return r.subIndex
}
