package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type accountTransitionRuntimeStore interface {
	RecordAccountTransitionRuntime(context.Context, string, domain.AccountTransitionRuntimeState, string, string, time.Time) (bool, error)
}

type accountTransitionRecoveryStore interface {
	ListUnsettledAccountTransitions(context.Context) ([]domain.SessionInterfaceTransition, error)
}

type accountTransitionRetirementStore interface {
	RetireAccountTransitionRuntime(context.Context, string, time.Time) (bool, error)
}

type accountTransitionRecoveryStatusStore interface {
	MarkAccountRecoveryUnconfirmed(context.Context, string, string, time.Time) (bool, error)
}

func (m *Manager) retireAccountTransitionRuntime(ctx context.Context, id string) (bool, error) {
	store, ok := m.store.(accountTransitionRetirementStore)
	if !ok {
		return false, nil
	}
	// The store rechecks all retirement proofs in the same statement as the
	// receipt update. Neither a stale latest-row read nor a reused handle owns
	// the current controller; lifecycle owners use this same SQL projection.
	return store.RetireAccountTransitionRuntime(ctx, id, m.clock())
}

type accountTransitionRuntimeContextKey struct{}

func (m *Manager) recordAccountRuntime(ctx context.Context, state domain.AccountTransitionRuntimeState, launchID, handleID string) error {
	id, _ := ctx.Value(accountTransitionRuntimeContextKey{}).(string)
	if id == "" {
		return nil
	}
	store, ok := m.store.(accountTransitionRuntimeStore)
	if !ok {
		return ErrInterfaceHandoffUnsupported
	}
	// Preserve receipts even if Create outlived a request or step deadline.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	changed, err := store.RecordAccountTransitionRuntime(writeCtx, id, state, launchID, handleID, m.clock())
	if err != nil {
		return ports.ErrAgentAccountStorageUnavailable
	}
	if !changed {
		return ErrInterfaceTransitionInProgress
	}
	return nil
}

// accountRuntimeCreated captures the actual/possible handle before session
// metadata commits. A failed receipt write must clean that exact handle; an
// unknown effect remains reserved and recovery cannot assume it is dead.
func (m *Manager) accountRuntimeCreated(ctx context.Context, launchID string, handle ports.RuntimeHandle, createErr error) error {
	if id, _ := ctx.Value(accountTransitionRuntimeContextKey{}).(string); id == "" {
		return createErr
	}
	if createErr != nil {
		var effect ports.RuntimeEffectError
		if errors.As(createErr, &effect) {
			if effect.EffectOutcome() == ports.RuntimeEffectNone || effect.CleanupOutcome() == ports.RuntimeCleanupSucceeded {
				return errors.Join(createErr, m.recordAccountRuntime(ctx, domain.AccountTransitionRuntimeStopped, launchID, ""))
			}
			handle = effect.PossibleHandle()
		}
		if handle.ID == "" {
			return createErr
		}
	}
	err := m.recordAccountRuntime(ctx, domain.AccountTransitionRuntimeStarted, launchID, handle.ID)
	if err == nil {
		return createErr
	}
	if handle.ID != "" && m.destroySpawnRuntimeAfterFailure(ctx, handle) {
		_ = m.recordAccountRuntime(ctx, domain.AccountTransitionRuntimeStopped, launchID, handle.ID)
	}
	return errors.Join(createErr, err)
}

func (m *Manager) stopAccountTransitionRuntime(ctx context.Context, transition domain.SessionInterfaceTransition) error {
	store, ok := m.store.(interfaceTransitionStore)
	if !ok {
		return ErrInterfaceHandoffUnsupported
	}
	latest, found, err := store.GetSessionInterfaceTransition(ctx, transition.ID)
	if err != nil || !found {
		return ports.ErrAgentAccountStorageUnavailable
	}
	if latest.Phase == domain.SessionInterfaceTransitionSourceStopped || latest.TargetRuntimeState == domain.AccountTransitionRuntimeNotStarted || latest.TargetRuntimeState == domain.AccountTransitionRuntimeStopped {
		return nil
	}
	if latest.Phase == domain.SessionInterfaceTransitionRecovery {
		retired, err := m.retireAccountTransitionRuntime(ctx, latest.ID)
		if err != nil || retired {
			return err
		}
	}
	if latest.TargetRuntimeState != domain.AccountTransitionRuntimeStarted || latest.TargetRuntimeHandleID == "" || latest.TargetRuntimeLaunchID == "" {
		// A legacy/partial receipt cannot identify one opaque handle. Only the
		// runtime adapter can conclusively close its whole session namespace.
		stopper, ok := m.runtime.(ports.SessionRuntimeStopper)
		if !ok {
			return errors.New("target runtime ownership is unknown; a runtime namespace shutdown is required before retrying recovery")
		}
		if err := stopper.StopSessionRuntimes(ctx, latest.SessionID); err != nil {
			return err
		}
		ctx = context.WithValue(ctx, accountTransitionRuntimeContextKey{}, latest.ID)
		return m.recordAccountRuntime(ctx, domain.AccountTransitionRuntimeStopped, latest.TargetRuntimeLaunchID, latest.TargetRuntimeHandleID)
	}
	// The receipt, not the old session metadata, identifies this target.
	rec, found, err := m.store.GetSession(ctx, latest.SessionID)
	if err != nil || !found {
		return ports.ErrAgentAccountStorageUnavailable
	}
	rec.Mode = domain.SessionModeTUI
	rec.Metadata.RuntimeHandleID = latest.TargetRuntimeHandleID
	rec.Metadata.RuntimeLaunchID = latest.TargetRuntimeLaunchID
	if err := m.stopSourceControllerConclusive(rec); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, accountTransitionRuntimeContextKey{}, latest.ID)
	return m.recordAccountRuntime(ctx, domain.AccountTransitionRuntimeStopped, latest.TargetRuntimeLaunchID, latest.TargetRuntimeHandleID)
}

// recoverTerminalAccountTransition closes an old recovery_required receipt
// without deleting historical attribution. Unknown shutdown keeps only this
// session fenced, and a later recovery retries the same durable obligation.
func (m *Manager) recoverTerminalAccountTransition(ctx context.Context, t domain.SessionInterfaceTransition) error {
	rec, found, err := m.store.GetSession(ctx, t.SessionID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	if !rec.IsTerminated && m.agentAccounts != nil {
		bound, err := m.agentAccounts.SessionAccountID(ctx, rec.ID, rec.Harness)
		if err != nil {
			return err
		}
		if bound == t.TargetAccountID {
			return nil
		}
	}
	retired, err := m.retireAccountTransitionRuntime(ctx, t.ID)
	if err != nil || retired {
		return err
	}
	if err := m.stopTransitionTargetConclusive(ctx, t); err != nil {
		m.logger.Warn("account transition: recovery shutdown unconfirmed; stop the session runtime and retry recovery", "sessionID", t.SessionID, "transition", t.ID, "errorType", fmt.Sprintf("%T", err))
		return nil
	}
	if t.TargetMode == domain.SessionModeChat {
		writeCtx := context.WithValue(ctx, accountTransitionRuntimeContextKey{}, t.ID)
		if err := m.recordAccountRuntime(writeCtx, domain.AccountTransitionRuntimeStopped, t.TargetRuntimeLaunchID, t.TargetRuntimeHandleID); err != nil {
			return err
		}
	}
	return nil
}

const accountRecoveryUnconfirmedDetail = "AO could not confirm the old account controller stopped. AO will automatically retry recovery the next time it starts."

// Persist a fixed classification for obligations which remain fenced. This
// does not broaden the older provider-error paths or record their contents.
func (m *Manager) markAccountRecoveryUnconfirmed(ctx context.Context, t domain.SessionInterfaceTransition) error {
	if t.ErrorCode == "TARGET_STOP_UNCONFIRMED" && t.ErrorDetail == accountRecoveryUnconfirmedDetail {
		return nil
	}
	store, ok := m.store.(accountTransitionRecoveryStatusStore)
	if !ok {
		return ErrInterfaceHandoffUnsupported
	}
	changed, err := store.MarkAccountRecoveryUnconfirmed(ctx, t.ID, accountRecoveryUnconfirmedDetail, m.clock())
	if err != nil {
		return err
	}
	if !changed {
		return ErrInterfaceTransitionInProgress
	}
	return nil
}
