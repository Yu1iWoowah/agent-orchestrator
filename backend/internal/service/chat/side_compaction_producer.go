package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/nativehome"
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
	if scope.NativeHomeError != "" {
		return r, fmt.Errorf("original provider native home: %s", scope.NativeHomeError)
	}
	if scope.NativeHome == "" {
		// These adapters rewrite the frozen environment at provider launch.
		// Without the original host's facts, another home is not a safe key.
		switch runtime.side.Harness {
		case domain.HarnessKimi, domain.HarnessCommandCode, domain.HarnessOpenCodeV2:
			return r, fmt.Errorf("original provider native home unavailable for %s", runtime.side.Harness)
		}
		r.NativeHomeSource = "launch_config"
		scope.NativeHome, err = nativehome.Resolve(ctx, runtime.side.Harness, frozen.Env, frozen.WorkspacePath, frozen.DataDir)
		if err != nil {
			return r, err
		}
	}
	r.ProviderHostID = runtime.side.ProviderHostID
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
