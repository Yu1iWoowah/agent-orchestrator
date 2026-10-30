package sessionmanager

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

type replacementWorkerNoticeStore interface {
	ports.ReplacementWorkerNoticeStore
	GetOrchestratorReplacement(context.Context, string) (domain.OrchestratorReplacement, bool, error)
}
type replacementWorkerChatNotifier interface {
	DeliverReplacementChatWorkerFYI(context.Context, string, domain.SessionID, string, int64) error
}

// DeliverReplacementWorkerFYI holds no project gate during worker I/O. The
// worker's existing input/controller lease protects its own runtime only.
func (m *Manager) DeliverReplacementWorkerFYI(ctx context.Context, expected domain.OrchestratorReplacement, target domain.SessionID) error {
	authority, ok := m.store.(replacementWorkerNoticeStore)
	if !ok {
		return domain.ErrReplacementStateInvalid
	}
	op, found, err := authority.GetOrchestratorReplacement(ctx, expected.ID)
	if err != nil {
		return err
	}
	if !found || op.Revision != expected.Revision || op.ProjectID != expected.ProjectID || op.Phase != domain.ReplacementNotifying {
		return domain.ErrReplacementSourceChanged
	}
	worker, found, err := m.store.GetSession(ctx, target)
	if err != nil {
		return err
	}
	if !found {
		return domain.ErrReplacementStateInvalid
	}
	if worker.Kind != domain.KindWorker || worker.ProjectID != op.ProjectID {
		return domain.ErrReplacementSourceChanged
	}
	if worker.IsTerminated || worker.IsTaskPreparation {
		return nil
	}
	if worker.HibernatedAt != nil || worker.ProvisionState != domain.SessionProvisionReady {
		if domain.NormalizeSessionMode(worker.Mode) != domain.SessionModeChat {
			return nil
		}
		queue, ok := m.store.(ports.ReplacementDormantWorkerNoticeQueue)
		if !ok {
			return ports.ErrChatUnsupported
		}
		_, err := queue.QueueDormantReplacementWorkerNotice(ctx, op.ID, op.Revision, target, worker.ControllerOwner(), m.clock())
		return err
	}
	if domain.NormalizeSessionMode(worker.Mode) == domain.SessionModeTUI {
		if worker.Activity.State != domain.ActivityIdle {
			return nil
		}
		alive, err := m.exactLaunchAlive(ctx, ports.RuntimeHandle{ID: worker.Metadata.RuntimeHandleID}, target, worker.Metadata.RuntimeLaunchID)
		if err != nil || !alive {
			return err
		}
		// Early read avoids consuming attempts for an ordinary human draft.
		// The input-lease check repeats this immediately before the write.
		if !m.noticeComposerClear(ctx, worker, m.lastTerminalInputAt(worker)) {
			return nil
		}
	}
	claim, send, err := authority.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, target, worker.ControllerOwner(), m.clock())
	if err != nil || !send {
		return err
	}
	attempted := false
	if claim.Mode == domain.SessionModeChat {
		chat, ok := m.chat.(replacementWorkerChatNotifier)
		if !ok {
			err = ports.ErrChatUnsupported
		} else {
			attempted = true // Canonical acceptance may precede an error response.
			err = chat.DeliverReplacementChatWorkerFYI(ctx, op.ID, target, claim.AttemptID, claim.Revision)
		}
	} else if m.messenger != nil {
		handle := ports.RuntimeHandle{ID: worker.Metadata.RuntimeHandleID}
		exact := m.exactLaunchPreWrite(target, worker.Harness, handle, claim.Owner.RuntimeLaunchID, domain.ErrReplacementSourceChanged)
		proof := func(ctx context.Context, current domain.SessionRecord) error {
			if current.Activity.State != domain.ActivityIdle || current.ControllerOwner() != claim.Owner {
				return domain.ErrReplacementSourceBusy
			}
			if _, _, _, err := authority.ValidateReplacementWorkerNotice(ctx, op.ID, target, claim.AttemptID, claim.Revision); err != nil {
				return err
			}
			if err := exact(ctx, current); err != nil {
				return err
			}
			// The fenced probe already settled preceding keystrokes. Do not
			// reenter the terminal input port while its fence is held.
			if !m.noticeComposerClear(ctx, current, time.Time{}) {
				return domain.ErrReplacementSourceBusy
			}
			return nil
		}
		var outcome sessionguard.Outcome
		outcome, err = m.messenger.DeliverWithOwnershipCheck(ctx, target, domain.WrapSessionDelivery(claim.NativeDeliveryID(), op.WorkerFYI()), proof, m.noticeComposerBusyCheck(), nil)
		attempted = outcome == sessionguard.Sent
	}
	// A cancelled caller is not evidence that a write was unattempted. Preserve
	// the guard's positive outcome using a bounded independent persistence call.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, finishErr := authority.FinishReplacementWorkerNotice(finishCtx, op.ID, target, claim.AttemptID, claim.Revision, attempted, m.clock())
	return errors.Join(err, finishErr)
}
