package chat

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/nativehome"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ErrNativeConversationProtected requires closing the side before native reuse.
var ErrNativeConversationProtected = ports.ErrNativeConversationProtected

// ErrNativeHomeUnavailable means a possibly protected native ID cannot be mapped safely.
var ErrNativeHomeUnavailable = errors.New("cannot determine the native conversation home")

type nativeKey struct{ home, id string }

// NativeGuard serializes AO operations with durable side compaction registration.
// Its callbacks must not acquire side or controller locks.
type NativeGuard struct {
	store ports.SideCompactionStore
	mu    sync.Mutex
	keys  map[nativeKey]*sync.Mutex
	homes map[string]*sync.RWMutex
}

// NewNativeGuard creates the daemon-wide native operation guard.
func NewNativeGuard(store ports.SideCompactionStore) *NativeGuard {
	return &NativeGuard{store: store, keys: map[nativeKey]*sync.Mutex{}, homes: map[string]*sync.RWMutex{}}
}

func (g *NativeGuard) homeLock(home string) *sync.RWMutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	lock := g.homes[home]
	if lock == nil {
		lock = &sync.RWMutex{}
		g.homes[home] = lock
	}
	return lock
}
func (g *NativeGuard) lock(home, id string) func() {
	homeLock := g.homeLock(home)
	g.mu.Lock()
	key := nativeKey{home, id}
	keyLock := g.keys[key]
	if keyLock == nil {
		keyLock = &sync.Mutex{}
		g.keys[key] = keyLock
	}
	g.mu.Unlock()
	homeLock.RLock()
	keyLock.Lock()
	return func() { keyLock.Unlock(); homeLock.RUnlock() }
}

// Run guards an existing native ID at the driver/conversation boundary.
func (g *NativeGuard) Run(ctx context.Context, provider domain.AgentHarness, cfg ports.ChatResumeConfig, call func() error) error {
	id := cfg.ProviderConversationID
	if g == nil || g.store == nil || id == "" {
		return call()
	}
	rows, err := g.store.SideCompactions(ctx, true)
	if err != nil {
		return err
	}
	env, resolveErr := nativehome.InvocationEnvironment(provider, cfg.Env, cfg.DataDir, cfg.SessionID)
	var home string
	if resolveErr == nil {
		home, resolveErr = nativehome.ResolveForOperation(ctx, provider, env, cfg.WorkspacePath, cfg.DataDir)
	}
	// A live host keeps its original canonical root even if an alias moved.
	for _, row := range rows {
		if row.ProviderHostID != "" && row.ProviderHostID == string(cfg.SessionID) && row.NativeID == id {
			home, resolveErr = row.NativeHome, nil
			break
		}
	}
	if resolveErr != nil {
		return fmt.Errorf("%w: %w", ErrNativeHomeUnavailable, resolveErr)
	}
	return g.runAtHome(ctx, home, cfg, call)
}

func (g *NativeGuard) runAtHome(ctx context.Context, home string, cfg ports.ChatResumeConfig, call func() error) error {
	id := cfg.ProviderConversationID
	if g == nil || g.store == nil || id == "" {
		return call()
	}
	release := g.lock(home, id)
	defer release()
	rows, err := g.store.SideCompactions(ctx, true)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.NativeHome == home && row.NativeID == id {
			if cfg.ReconnectOnly && !row.Historical && row.ProviderHostID == string(cfg.SessionID) && row.ID == ports.ChatCompactionID(ctx) && row.Epoch == ports.ChatCompactionEpoch(ctx) {
				continue
			}
			return fmt.Errorf("%w: side %s", ErrNativeConversationProtected, row.SideID)
		}
	}
	return call()
}

// DeleteHome prevents account deletion racing with registration or native use.
func (g *NativeGuard) DeleteHome(ctx context.Context, home string, call func() error) error {
	if g == nil || g.store == nil {
		return call()
	}
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil {
		return err
	}
	home = canonical
	lock := g.homeLock(home)
	lock.Lock()
	defer lock.Unlock()
	rows, err := g.store.SideCompactions(ctx, true)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.NativeHome == home {
			return fmt.Errorf("%w: side %s", ErrNativeConversationProtected, row.SideID)
		}
	}
	return call()
}
