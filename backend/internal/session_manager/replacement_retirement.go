package sessionmanager

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementRetirementStore interface {
	CommitReplacementRetirement(context.Context, string, int64, domain.SessionRecord, int64, func(context.Context) error) (domain.OrchestratorReplacement, bool, error)
}

// RetireReplacementSource preserves every workspace and worker. Shutdown has
// already been confirmed before export; this path never repeats it or invokes
// generic termination, native-session cleanup, container reaping or stashing.
func (m *Manager) RetireReplacementSource(ctx context.Context, op domain.OrchestratorReplacement, proof ports.ReplacementRetirementProof) (domain.OrchestratorReplacement, error) {
	var zero domain.OrchestratorReplacement
	if op.Phase != domain.ReplacementRetiring || proof == nil || proof.OperationID() != op.ID || proof.OperationRevision() != op.Revision || proof.DocumentRevision() <= 0 {
		return zero, domain.ErrReplacementStateInvalid
	}
	ctx, rec, release, err := m.beginReplacementSourceAction(ctx, op)
	if err != nil {
		return zero, err
	}
	defer release()
	verified := proof.Source()
	if verified.ID != rec.ID || verified.ControllerOwner() != rec.ControllerOwner() || verified.Metadata.RuntimeHandleID != rec.Metadata.RuntimeHandleID || verified.Metadata.BrowserCapabilityVerifier != rec.Metadata.BrowserCapabilityVerifier {
		return zero, domain.ErrReplacementSourceChanged
	}
	store, ok := m.store.(replacementRetirementStore)
	if !ok {
		return zero, domain.ErrReplacementStateInvalid
	}
	usage, ok := m.lcm.(interface {
		FinalizeReplacementUsage(context.Context, domain.SessionRecord) error
	})
	if !ok {
		return zero, domain.ErrReplacementStateInvalid
	}
	if err := m.beginAgentOperation(ctx, rec.ID, agentOperationRetire); err != nil {
		return zero, err
	}
	defer m.endAgentOperation(rec.ID, agentOperationRetire)
	if err := m.confirmReplacementSourceStopped(ctx, op, rec); err != nil {
		return zero, err
	}
	if err := proof.Check(ctx); err != nil {
		return zero, err
	}
	if err := usage.FinalizeReplacementUsage(ctx, rec); err != nil {
		return zero, err
	}
	_, current, releaseCheck, err := m.beginReplacementSourceAction(ctx, op)
	if err != nil {
		return zero, err
	}
	defer releaseCheck()
	if err := m.confirmReplacementSourceStopped(ctx, op, current); err != nil {
		return zero, err
	}
	// The store rechecks operation, controller, frozen writer and author/evidence
	// receipts under its global reservation. No provider I/O runs in that lock.
	next, applied, err := store.CommitReplacementRetirement(ctx, op.ID, op.Revision, rec, proof.DocumentRevision(), proof.Check)
	if err != nil {
		return zero, err
	}
	if !applied {
		return zero, domain.ErrReplacementSourceChanged
	}
	// Shell teardown is an irreversible UI/process side effect. It must not
	// precede the retirement transaction, which can still reject current proof.
	cleanup, cancel := spawnRollbackContext(ctx)
	defer cancel()
	closeRelease, err := m.beginShellTerminalTeardown(cleanup, rec.ID)
	if err != nil {
		m.logger.Warn("source shell cleanup after replacement retirement", "sessionID", rec.ID, "error", err)
		return next, err
	}
	if closeRelease != nil {
		closeRelease()
	}
	return next, nil
}

func (m *Manager) confirmReplacementSourceStopped(ctx context.Context, op domain.OrchestratorReplacement, rec domain.SessionRecord) error {
	if op.SourceStop == nil || !op.SourceStop.MatchesSource(rec) {
		return domain.ErrReplacementStateInvalid
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		if m.chat == nil || m.chat.HasLiveChatController(rec.ID) {
			return ports.ErrChatRecoveryInconclusive
		}
		return ctx.Err()
	}
	if rec.Metadata.RuntimeHandleID == "" || rec.Metadata.RuntimeLaunchID == "" {
		return ErrIncompleteHandle
	}
	ref := ports.FencedRuntimeRef{Handle: runtimeHandle(rec.Metadata), SessionID: rec.ID, Generation: rec.Metadata.RuntimeLaunchID, NativeIdentity: rec.Metadata.AgentSessionID}
	if m.runtime.ProbeFencedRuntime(ctx, ref).Liveness != ports.FencedDead {
		return tuiEvidenceError("ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN", "Original source runtime is not positively dead")
	}
	return ctx.Err()
}
