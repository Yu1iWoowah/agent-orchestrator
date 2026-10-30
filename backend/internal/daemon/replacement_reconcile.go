package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacementsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

const replacementReconcileTick = 2 * time.Second

type replacementSessionRuntime interface {
	replacementsvc.Runtime
	BeginReplacementMutation(context.Context, domain.SessionID) (context.Context, func(), error)
	WithOrchestratorProjectGate(context.Context, domain.ProjectID, func(context.Context) error) error
}

var _ replacementSessionRuntime = (*sessionmanager.Manager)(nil)

// runReplacementReconciliation never races boot-time host/controller recovery.
// Its caller owns the ticker and joins this loop before closing dependencies.
func runReplacementReconciliation(ctx context.Context, startup <-chan struct{}, ticks <-chan time.Time, reconcile func(context.Context) error, log *slog.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-startup:
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if err := reconcile(ctx); err != nil && ctx.Err() == nil {
			log.Warn("orchestrator replacement reconciliation deferred", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case _, open := <-ticks:
			if !open {
				return
			}
		}
	}
}
