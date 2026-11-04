package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (m *sideManager) registerCompaction(ctx context.Context, runtime *sideRuntime) error {
	store, ok := m.store.(*memorySideStore)
	if !ok || store.durable == nil {
		return nil
	}
	var frozen frozenSideConfig
	if err := json.Unmarshal(runtime.side.LaunchConfig, &frozen); err != nil {
		return err
	}
	var scope ports.ChatCompactionScope
	if reader, ok := runtime.conv.(ports.ChatCompactionScopeReader); ok {
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		scope, _ = reader.CompactionScope(queryCtx)
		cancel()
	}
	if scope.NativeHome == "" {
		key, fallback := "CODEX_HOME", ".codex"
		if runtime.side.Harness != domain.HarnessCodex {
			key, fallback = "CLAUDE_CONFIG_DIR", ".claude"
		}
		home := frozen.Env[key]
		if home == "" {
			base := frozen.Env["HOME"]
			if base == "" {
				base = frozen.Env["USERPROFILE"]
			}
			if base == "" {
				base, _ = os.UserHomeDir()
			}
			home = filepath.Join(base, fallback)
		}
		if !filepath.IsAbs(home) {
			home = filepath.Join(frozen.WorkspacePath, home)
		}
		var err error
		scope.NativeHome, err = filepath.EvalSymlinks(home)
		if err != nil {
			return err
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	raw, err := store.contentLocked(runtime.side.ID)
	if err != nil {
		return err
	}
	r := domain.SideCompaction{ID: runtime.compactionID, Epoch: runtime.compactionEpoch, SideID: runtime.side.ID, SessionID: runtime.side.SessionID, NativeHome: scope.NativeHome, NativeID: runtime.conv.ProviderConversationID(), Provider: runtime.side.Harness, PermissionMode: frozen.Permissions, ReadOnly: frozen.ReadOnly, Scope: scope.Processes, State: domain.SideCompactionPending, Content: raw}
	if err := store.durable.RegisterSideCompaction(ctx, r); err != nil {
		return err
	}
	if store.protected == nil {
		store.protected = map[string]bool{}
	}
	store.protected[r.SideID] = true
	return nil
}

func (m *sideManager) transitionCompaction(ctx context.Context, runtime *sideRuntime, state domain.SideCompactionState) error {
	store, ok := m.store.(*memorySideStore)
	if !ok || store.durable == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.durable.TransitionSideCompaction(ctx, runtime.compactionID, runtime.compactionEpoch, state, false, false); err != nil {
		return err
	}
	if state == domain.SideCompactionSettled || state == domain.SideCompactionNotSent {
		delete(store.protected, runtime.side.ID)
	}
	return nil
}
