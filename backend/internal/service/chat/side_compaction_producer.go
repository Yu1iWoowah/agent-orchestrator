package chat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (m *sideManager) prepareCompaction(ctx context.Context, runtime *sideRuntime) (r domain.SideCompaction, err error) {
	store, ok := m.store.(*memorySideStore)
	if !ok || store.durable == nil {
		return r, nil
	}
	var frozen frozenSideConfig
	if err := json.Unmarshal(runtime.side.LaunchConfig, &frozen); err != nil {
		return r, err
	}
	r.ScopeSource, r.NativeHomeSource = "not_reported", "host"
	var scope ports.ChatCompactionScope
	if reader, ok := runtime.conv.(ports.ChatCompactionScopeReader); ok {
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		scope, err = reader.CompactionScope(queryCtx)
		cancel()
		r.ScopeSource = "host"
		if err != nil {
			r.ScopeSource = "host_error"
			if errors.Is(err, context.DeadlineExceeded) {
				r.ScopeSource = "host_timeout"
			}
			m.service.log.Warn("side compaction scope unavailable", "side", runtime.side.ID, "code", "CHAT_SIDE_COMPACTION_SCOPE_FAILED", "error", err)
			scope = ports.ChatCompactionScope{}
		} else if scope.Processes == nil {
			r.ScopeSource = "host_missing_identity"
		}
	}
	if scope.NativeHome == "" {
		r.NativeHomeSource = "launch_config"
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
			return r, err
		}
	}
	r.SideID, r.SessionID, r.NativeID = runtime.side.ID, runtime.side.SessionID, runtime.conv.ProviderConversationID()
	r.NativeHome, r.Provider, r.PermissionMode, r.ReadOnly, r.Scope, r.State = scope.NativeHome, runtime.side.Harness, frozen.Permissions, frozen.ReadOnly, scope.Processes, domain.SideCompactionPending
	return r, nil
}

func (m *sideManager) registerCompaction(ctx context.Context, r domain.SideCompaction) error {
	store, ok := m.store.(*memorySideStore)
	if !ok || store.durable == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	raw, err := store.contentLocked(r.SideID)
	if err != nil {
		return err
	}
	r.Content = raw
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
