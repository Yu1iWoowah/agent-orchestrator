package orchestratorreplacement

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Notification transport has its own durable claim and never runs under the
// project's lifecycle gate. A notification failure cannot undo released intake.
type workerNotifier interface {
	DeliverReplacementWorkerFYI(context.Context, domain.OrchestratorReplacement, domain.SessionID) error
}

func (s *Service) reconcileNotifications(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	current, _, err := s.deps.Store.ReconcileReplacementNotificationReceipts(ctx, op.ID, op.Revision, s.deps.Now().UTC())
	if err != nil {
		return op, err
	}
	op = current
	if op.Phase != domain.ReplacementNotifying {
		return op, nil
	}
	notifier, ok := s.deps.Runtime.(workerNotifier)
	if !ok {
		return op, nil
	}
	receipts, err := s.deps.Store.ReplacementWorkerNoticeReceipts(ctx, op.ID)
	if err != nil {
		return op, err
	}
	if len(receipts) != len(op.Notifications) || len(receipts) == 0 {
		return op, domain.ErrReplacementStateInvalid
	}
	eligible := receipts[:0]
	for _, receipt := range receipts {
		if receipt.State == domain.ReplacementNoticeAccepted || receipt.State == domain.ReplacementNoticeSkipped || receipt.Mode == domain.SessionModeTUI && receipt.State == domain.ReplacementNoticeUncertain {
			continue
		}
		eligible = append(eligible, receipt)
	}
	receipts = eligible
	if len(receipts) == 0 {
		return op, nil
	}
	// This cursor only schedules fair bounded work. Store claims, canonical IDs,
	// and receipts provide safety across concurrent callers and daemon restarts.
	batch, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for range min(16, len(receipts)) {
		if batch.Err() != nil {
			break
		}
		s.noticeMu.Lock()
		if s.noticeCursor == nil {
			s.noticeCursor = make(map[string]int)
		}
		index := s.noticeCursor[op.ID] % len(receipts)
		s.noticeCursor[op.ID] = (index + 1) % len(receipts)
		s.noticeMu.Unlock()
		receipt := receipts[index]
		// Delivery records no-write/uncertain outcomes; accepted evidence can
		// still be recovered below after a lost transport response.
		func() {
			target, cancel := context.WithTimeout(batch, 5*time.Second)
			defer cancel()
			_ = notifier.DeliverReplacementWorkerFYI(target, op, receipt.SessionID)
		}()
	}
	if ctx.Err() != nil {
		return op, ctx.Err()
	}
	current, _, err = s.deps.Store.ReconcileReplacementNotificationReceipts(ctx, op.ID, op.Revision, s.deps.Now().UTC())
	if err != nil {
		return op, err
	}
	if current.Phase == domain.ReplacementCompleted {
		s.noticeMu.Lock()
		delete(s.noticeCursor, op.ID)
		s.noticeMu.Unlock()
	}
	return current, nil
}
