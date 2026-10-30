package sessionmanager

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (m *Manager) quiesceReplacementTUISource(ctx context.Context, op domain.OrchestratorReplacement, rec domain.SessionRecord) (stop domain.ReplacementControllerStop, resultErr error) {
	if op.FrozenAt == nil || op.SourceStopIntentAt == nil || op.Preparation == nil || op.Preparation.VerifiedAt == nil || op.Transcript == nil {
		return stop, domain.ErrReplacementStateInvalid
	}
	if rec.Metadata.RuntimeLaunchID == "" || rec.Metadata.RuntimeHandleID == "" || rec.Metadata.AgentSessionID == "" || rec.Metadata.AgentSessionIDLaunchID != rec.Metadata.RuntimeLaunchID {
		return stop, ErrIncompleteHandle
	}
	store, ok := m.store.(interface {
		SetReplacementTUIRuntimeReleased(context.Context, string, int64, domain.SessionID, int64) (bool, error)
		ClearUnattemptedReplacementTUIRelease(context.Context, string, int64, domain.SessionID, string) (bool, error)
	})
	if !ok {
		return stop, domain.ErrReplacementStateInvalid
	}
	if err := m.beginAgentOperation(ctx, rec.ID, agentOperationExit); err != nil {
		return stop, err
	}
	defer m.endAgentOperation(rec.ID, agentOperationExit)
	active, err := m.hasActiveInterfaceTransition(ctx, rec.ID)
	if err != nil {
		return stop, err
	}
	if active {
		return stop, domain.ErrReplacementSourceBusy
	}
	_, rec, release, err := m.beginReplacementSourceAction(ctx, op)
	if err != nil {
		return stop, err
	}
	defer release()
	ref := ports.FencedRuntimeRef{Handle: runtimeHandle(rec.Metadata), SessionID: rec.ID, Generation: rec.Metadata.RuntimeLaunchID, NativeIdentity: rec.Metadata.AgentSessionID}
	if rec.Metadata.RuntimeReleasedLaunchID != ref.Generation {
		observation, err := m.observeReplacementTUISource(ctx, op, rec)
		if err != nil {
			return stop, err
		}
		if observation.State != ports.ChatControllerReady || observation.PendingApproval || observation.ControlTurn == nil || observation.ControlTurn.State != domain.TurnStateCompleted {
			return stop, domain.ErrReplacementSourceBusy
		}

		attempted := false
		defer func() {
			if attempted {
				return
			}
			cleanup, cancel := spawnRollbackContext(ctx)
			defer cancel()
			cleared, err := store.ClearUnattemptedReplacementTUIRelease(cleanup, op.ID, op.Revision, rec.ID, ref.Generation)
			if err != nil || !cleared {
				// Do not wrap the original Busy: the coordinator must retain the
				// stop intent when its no-stop clearance could not be committed.
				resultErr = tuiEvidenceError("ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN", fmt.Sprintf("Unattempted TUI release clearance is pending: %v (cleared=%t)", err, cleared))
			} else if resultErr != nil {
				resultErr = fmt.Errorf("%w: no Destroy attempted: %w", domain.ErrReplacementSourceBusy, resultErr)
			}
		}()
		reserved, err := store.SetReplacementTUIRuntimeReleased(ctx, op.ID, op.Revision, rec.ID, rec.Revision)
		if err != nil {
			return stop, err
		}
		if !reserved {
			return stop, domain.ErrReplacementSourceBusy
		}
		_, current, releaseCheck, err := m.beginReplacementSourceAction(ctx, op)
		if err != nil {
			return stop, err
		}
		releaseCheck()
		if current.Metadata.RuntimeHandleID != ref.Handle.ID || current.Metadata.RuntimeReleasedLaunchID != ref.Generation {
			return stop, domain.ErrReplacementSourceChanged
		}
		if current.Activity.State != domain.ActivityIdle {
			return stop, domain.ErrReplacementSourceBusy
		}
		if err := m.exactLaunchPreWrite(rec.ID, rec.Harness, ref.Handle, ref.Generation, domain.ErrReplacementSourceChanged)(ctx, current); err != nil {
			return stop, err
		}
		// Destroy success is only an acknowledgement. Confirm the same launch
		// below even when Destroy returns nil; never blindly repeat a stop.
		attempted = true
		if err := m.runtime.Destroy(ctx, ref.Handle); err != nil {
			if probe := m.runtime.ProbeFencedRuntime(ctx, ref); probe.Liveness != ports.FencedDead {
				return stop, fmt.Errorf("replacement TUI shutdown: %w", err)
			}
		}
	}
	if probe := m.runtime.ProbeFencedRuntime(ctx, ref); probe.Liveness != ports.FencedDead {
		return stop, tuiEvidenceError("ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN", "Original TUI launch is not positively dead")
	}
	_, current, releaseCheck, err := m.beginReplacementSourceAction(ctx, op)
	if err != nil {
		return stop, err
	}
	releaseCheck()
	if current.Metadata.RuntimeHandleID != ref.Handle.ID || current.Metadata.RuntimeReleasedLaunchID != ref.Generation {
		return stop, domain.ErrReplacementSourceChanged
	}
	if current.Activity.State != domain.ActivityExited {
		if err := m.recordAgentExited(ctx, current); err != nil {
			return stop, err
		}
	}
	_, current, releaseCheck, err = m.beginReplacementSourceAction(ctx, op)
	if err != nil {
		return stop, err
	}
	defer releaseCheck()
	stop = domain.ReplacementControllerStop{Owner: op.SourceOwner, CapabilityVerifier: op.SourceCapabilityVerifier, NativeID: ref.NativeIdentity, Method: "tui-fenced-stop", ConfirmedAt: m.clock().UTC()}
	if !stop.MatchesSource(current) || current.Metadata.RuntimeHandleID != ref.Handle.ID || m.runtime.ProbeFencedRuntime(ctx, ref).Liveness != ports.FencedDead {
		return domain.ReplacementControllerStop{}, tuiEvidenceError("ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN", "Original TUI shutdown facts changed before confirmation")
	}
	return stop, nil
}
