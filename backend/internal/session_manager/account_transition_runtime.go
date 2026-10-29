package sessionmanager

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type accountTransitionRuntimeStore interface {
	RecordAccountTransitionRuntime(context.Context, string, domain.AccountTransitionRuntimeState, string, string, time.Time) (bool, error)
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
	if latest.TargetRuntimeState != domain.AccountTransitionRuntimeStarted || latest.TargetRuntimeHandleID == "" || latest.TargetRuntimeLaunchID == "" {
		return errors.New("target runtime may have started before its ownership receipt was saved")
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
