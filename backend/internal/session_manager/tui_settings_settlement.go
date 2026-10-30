package sessionmanager

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type tuiSettingsSettlementStore interface {
	AgentSettingsOperation(context.Context, domain.SessionID, string) (domain.AgentSettingsOperation, error)
	ConfirmAbandonedTUISettingsWriter(context.Context, domain.SessionID, string, int64, time.Time) (domain.AgentSettingsOperation, error)
}

// SettleTUIAgentSettings is an explicit local operator cleanup, never a launch.
// The caller may consent to stopping an exact known generation, but cannot
// assert that an unknown writer is gone. Durable proof requires every possible
// handoff generation to be conclusively absent under the lifecycle/workspace gates.
func (m *Manager) SettleTUIAgentSettings(ctx context.Context, id domain.SessionID, operationID string, version int64, stopIfAlive bool) (domain.AgentSettingsOperation, error) {
	if version < 0 || operationID == "" {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsInvalid
	}
	if err := m.beginAgentOperation(ctx, id, agentOperationKill); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	defer m.endAgentOperation(id, agentOperationKill)
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	if !found || !rec.IsTerminated || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeTUI {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsConflict
	}
	release := m.acquireWorkspaceGate(rec.ProjectID)
	defer release()
	store, ok := m.store.(tuiSettingsSettlementStore)
	if !ok {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsUnsupported
	}
	op, err := store.AgentSettingsOperation(ctx, id, operationID)
	if err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	if op.Version != version || op.Phase != domain.AgentSettingsCancelled || op.ErrorCode != "AGENT_SETTINGS_SESSION_TERMINATED" || op.StartedAt == nil || op.TUI == nil || op.TUI.TerminationVerified || op.TUI.SourceRuntimeHandle == "" || op.ControllerGeneration == "" || op.TUI.TargetGeneration == "" {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsConflict
	}
	ref := func(handle, generation string) ports.FencedRuntimeRef {
		// Production TUI runtimes use the session ID as their canonical handle.
		// A lost Create receipt does not erase the pinned launch generation.
		if handle == "" {
			handle = string(id)
		}
		return ports.FencedRuntimeRef{Handle: ports.RuntimeHandle{ID: handle}, SessionID: id, Generation: generation, NativeIdentity: op.ProviderConversationID}
	}
	refs := []ports.FencedRuntimeRef{ref(op.TUI.SourceRuntimeHandle, op.ControllerGeneration), ref(op.TUI.TargetRuntimeHandle, op.TUI.TargetGeneration), ref(op.TUI.RollbackRuntimeHandle, op.ID+":rollback")}
	unconfirmed := func() (domain.AgentSettingsOperation, error) {
		if err := ctx.Err(); err != nil {
			return domain.AgentSettingsOperation{}, err
		}
		return domain.AgentSettingsOperation{}, apierr.Conflict("AGENT_SETTINGS_WRITER_UNCONFIRMED", "Native writer could not be confirmed stopped; workspace and launch veto are retained", nil)
	}
	// Different generations can share one runtime handle. First stop only exact
	// matches with explicit consent; then re-probe every generation. An unknown
	// or unrelated generation is never adopted or destroyed.
	for _, candidate := range refs {
		proof := m.runtime.ProbeFencedRuntime(ctx, candidate)
		if proof.Liveness == ports.FencedAlive && stopIfAlive {
			if err := m.stopSourceControllerConclusive(domain.SessionRecord{ID: id, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{RuntimeHandleID: candidate.Handle.ID}}); err != nil {
				return unconfirmed()
			}
		}
	}
	for _, candidate := range refs {
		proof := m.runtime.ProbeFencedRuntime(ctx, candidate)
		if proof.Liveness != ports.FencedDead || proof.Reason != ports.FencedReasonExactAbsent {
			return unconfirmed()
		}
	}
	if err := ctx.Err(); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	confirmed, err := store.ConfirmAbandonedTUISettingsWriter(ctx, id, operationID, version, m.clock())
	if err == nil {
		m.clearTUISettingsFence(id, operationID)
	}
	return confirmed, err
}
