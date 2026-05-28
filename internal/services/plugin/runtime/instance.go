package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

const (
	maxExecutionTime    = 10 * time.Millisecond
	maxMemoryPerInstance = 32 * 1024 * 1024
	maxActionsPerEvent  = 20
	circuitThreshold    = 5
	circuitCooldown     = 10 * time.Minute
	maxInstancePoolSize = 10
)

type CircuitState int

const (
	CircuitClosed CircuitState = iota
	CircuitOpen
	CircuitHalfOpen
)

type CircuitBreaker struct {
	mu             sync.Mutex
	failureCount   int
	state          CircuitState
	lastFailure    time.Time
	lastTransition time.Time
	communityID    uuid.UUID
	pluginID       uuid.UUID
}

type Instance struct {
	mu            sync.Mutex
	mod           api.Module
	runtime       wazero.Runtime
	pluginID      uuid.UUID
	communityID   uuid.UUID
	handleEvent   api.Function
	handleCommand api.Function
	initFn        api.Function
	destroyFn     api.Function
	circuit       *CircuitBreaker
	lastUsed      time.Time
}

type InstancePool struct {
	mu    sync.Mutex
	pool  map[string]chan *Instance
	store *StateStore
}

func NewInstancePool(store *StateStore) *InstancePool {
	return &InstancePool{
		pool:  make(map[string]chan *Instance),
		store: store,
	}
}

func (p *InstancePool) Get(ctx context.Context, pluginID, communityID uuid.UUID, wasmBytes []byte, host *HostFunctions) (*Instance, error) {
	key := fmt.Sprintf("%s:%s", pluginID, communityID)

	p.mu.Lock()
	ch, exists := p.pool[key]
	if !exists {
		ch = make(chan *Instance, maxInstancePoolSize)
		p.pool[key] = ch
	}
	p.mu.Unlock()

	select {
	case inst := <-ch:
		inst.mu.Lock()
		if inst.circuit.state == CircuitOpen {
			if time.Since(inst.circuit.lastTransition) >= circuitCooldown {
				inst.circuit.state = CircuitHalfOpen
			} else {
				inst.mu.Unlock()
				ch <- inst
				return nil, fmt.Errorf("circuit breaker open for plugin %s in community %s", pluginID, communityID)
			}
		}
		inst.mu.Unlock()

		if time.Since(inst.lastUsed) > 30*time.Second {
			inst.resetModule(ctx, wasmBytes, host)
		}

		inst.lastUsed = time.Now()
		return inst, nil
	default:
		inst, err := newInstance(ctx, pluginID, communityID, wasmBytes, host)
		if err != nil {
			return nil, err
		}
		inst.lastUsed = time.Now()
		return inst, nil
	}
}

func (p *InstancePool) Put(inst *Instance) {
	inst.lastUsed = time.Now()
	key := fmt.Sprintf("%s:%s", inst.pluginID, inst.communityID)

	p.mu.Lock()
	ch := p.pool[key]
	if ch == nil {
		ch = make(chan *Instance, maxInstancePoolSize)
		p.pool[key] = ch
	}
	p.mu.Unlock()

	select {
	case ch <- inst:
	default:
		inst.Close()
	}
}

func newInstance(ctx context.Context, pluginID, communityID uuid.UUID, wasmBytes []byte, host *HostFunctions) (*Instance, error) {
	wasmRuntime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithMemoryLimitPages(maxMemoryPerInstance / 65536))

	wasi_snapshot_preview1.MustInstantiate(ctx, wasmRuntime)

	buildHostModule(ctx, wasmRuntime, host, pluginID, communityID)

	mod, err := wasmRuntime.InstantiateWithConfig(ctx, wasmBytes,
		wazero.NewModuleConfig().WithName(fmt.Sprintf("plugin_%s", pluginID)))
	if err != nil {
		wasmRuntime.Close(ctx)
		return nil, fmt.Errorf("instantiate wasm: %w", err)
	}

	inst := &Instance{
		mod:           mod,
		runtime:       wasmRuntime,
		pluginID:      pluginID,
		communityID:   communityID,
		handleEvent:   mod.ExportedFunction("handleEvent"),
		handleCommand: mod.ExportedFunction("handleCommand"),
		initFn:        mod.ExportedFunction("init"),
		destroyFn:     mod.ExportedFunction("destroy"),
		circuit: &CircuitBreaker{
			communityID: communityID,
			pluginID:    pluginID,
		},
	}

	if inst.initFn != nil {
		initCtx, cancel := context.WithTimeout(ctx, maxExecutionTime)
		defer cancel()
		_, err = inst.initFn.Call(initCtx)
		if err != nil {
			inst.Close()
			return nil, fmt.Errorf("plugin init: %w", err)
		}
	}

	return inst, nil
}

func (i *Instance) HandleEvent(ctx context.Context, eventType string, data json.RawMessage) ([]Action, error) {
	i.mu.Lock()
	if i.circuit.state == CircuitOpen {
		if time.Since(i.circuit.lastTransition) >= circuitCooldown {
			i.circuit.state = CircuitHalfOpen
		} else {
			i.mu.Unlock()
			return nil, fmt.Errorf("circuit breaker open")
		}
	}
	i.mu.Unlock()

	if i.handleEvent == nil {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, maxExecutionTime)
	defer cancel()

	typeBytes := []byte(eventType)
	typePtr, typeLen := writeBytesToMemory(i.mod, typeBytes)
	dataPtr, dataLen := writeBytesToMemory(i.mod, data)

	resultCh := make(chan struct {
		results []uint64
		err     error
	}, 1)

	go func() {
		results, err := i.handleEvent.Call(ctx, uint64(typePtr), uint64(typeLen), uint64(dataPtr), uint64(dataLen))
		resultCh <- struct {
			results []uint64
			err     error
		}{results, err}
	}()

	select {
	case <-ctx.Done():
		i.recordFailure()
		return nil, nil
	case res := <-resultCh:
		if res.err != nil {
			log.Warn().Err(res.err).Str("plugin", i.pluginID.String()).Msg("plugin handleEvent failed")
			i.recordFailure()
			return nil, nil
		}

		i.recordSuccess()

		if len(res.results) == 0 || res.results[0] == 0 {
			return nil, nil
		}

		actions, err := readActionListFromMemory(i.mod, uint32(res.results[0]))
		if err != nil {
			return nil, err
		}

		if len(actions) > maxActionsPerEvent {
			actions = actions[:maxActionsPerEvent]
		}

		return actions, nil
	}
}

func (i *Instance) HandleCommand(ctx context.Context, name string, args json.RawMessage) ([]Action, error) {
	if i.handleCommand == nil {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, maxExecutionTime)
	defer cancel()

	nameBytes := []byte(name)
	namePtr, nameLen := writeBytesToMemory(i.mod, nameBytes)
	if args == nil {
		args = json.RawMessage("{}")
	}
	argsPtr, argsLen := writeBytesToMemory(i.mod, args)

	resultCh := make(chan struct {
		results []uint64
		err     error
	}, 1)

	go func() {
		results, err := i.handleCommand.Call(ctx, uint64(namePtr), uint64(nameLen), uint64(argsPtr), uint64(argsLen))
		resultCh <- struct {
			results []uint64
			err     error
		}{results, err}
	}()

	select {
	case <-ctx.Done():
		i.recordFailure()
		return nil, nil
	case res := <-resultCh:
		if res.err != nil {
			i.recordFailure()
			return nil, nil
		}

		i.recordSuccess()

		if len(res.results) == 0 || res.results[0] == 0 {
			return nil, nil
		}

		actions, err := readActionListFromMemory(i.mod, uint32(res.results[0]))
		if err != nil {
			return nil, err
		}

		if len(actions) > maxActionsPerEvent {
			actions = actions[:maxActionsPerEvent]
		}

		return actions, nil
	}
}

func (i *Instance) recordFailure() {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.circuit.failureCount++
	i.circuit.lastFailure = time.Now()

	if i.circuit.failureCount >= circuitThreshold {
		i.circuit.state = CircuitOpen
		i.circuit.lastTransition = time.Now()
		log.Warn().
			Str("plugin", i.pluginID.String()).
			Str("community", i.communityID.String()).
			Int("failures", i.circuit.failureCount).
			Msg("plugin circuit breaker opened")
	}
}

func (i *Instance) recordSuccess() {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.circuit.state == CircuitHalfOpen {
		i.circuit.state = CircuitClosed
		i.circuit.lastTransition = time.Now()
		log.Info().
			Str("plugin", i.pluginID.String()).
			Str("community", i.communityID.String()).
			Msg("plugin circuit breaker closed")
	}

	i.circuit.failureCount = 0
}

func (i *Instance) resetModule(ctx context.Context, wasmBytes []byte, host *HostFunctions) {
	i.Close()

	newInst, err := newInstance(ctx, i.pluginID, i.communityID, wasmBytes, host)
	if err != nil {
		return
	}

	i.mod = newInst.mod
	i.runtime = newInst.runtime
	i.handleEvent = newInst.handleEvent
	i.handleCommand = newInst.handleCommand
	i.initFn = newInst.initFn
	i.destroyFn = newInst.destroyFn
}

func (i *Instance) Close() {
	if i.destroyFn != nil {
		destroyCtx, cancel := context.WithTimeout(context.Background(), maxExecutionTime)
		_, _ = i.destroyFn.Call(destroyCtx)
		cancel()
	}
	if i.runtime != nil {
		i.runtime.Close(context.Background())
	}
}
