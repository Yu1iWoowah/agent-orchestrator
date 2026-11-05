package chat

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/nativehome"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ErrNativeConversationProtected requires closing the side before native reuse.
var ErrNativeConversationProtected = ports.ErrNativeConversationProtected

type nativeKey struct{ home, id string }

// NativeGuard serializes AO operations with durable side compaction registration.
// Its callbacks must not acquire side or controller locks.
type NativeGuard struct {
	store ports.SideCompactionStore
	keys  sync.Map
	homes sync.Map
}

func NewNativeGuard(store ports.SideCompactionStore) *NativeGuard {
	return &NativeGuard{store: store}
}

func (g *NativeGuard) lock(home, id string) func() {
	h, _ := g.homes.LoadOrStore(home, &sync.RWMutex{})
	homeLock := h.(*sync.RWMutex)
	homeLock.RLock()
	k, _ := g.keys.LoadOrStore(nativeKey{home, id}, &sync.Mutex{})
	keyLock := k.(*sync.Mutex)
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
	home, resolveErr := nativehome.Resolve(ctx, provider, cfg.Env, cfg.WorkspacePath, cfg.DataDir)
	// A live host keeps its original canonical root even if an alias moved.
	for _, row := range rows {
		if row.ProviderHostID != "" && row.ProviderHostID == string(cfg.SessionID) && row.NativeID == id {
			home, resolveErr = row.NativeHome, nil
			break
		}
	}
	if resolveErr != nil {
		if len(rows) == 0 {
			return call()
		}
		return fmt.Errorf("%w: %w", ErrNativeConversationProtected, resolveErr)
	}
	release := g.lock(home, id)
	defer release()
	rows, err = g.store.SideCompactions(ctx, true)
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
	h, _ := g.homes.LoadOrStore(home, &sync.RWMutex{})
	lock := h.(*sync.RWMutex)
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
