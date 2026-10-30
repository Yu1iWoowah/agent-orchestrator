package orchestratorreplacement_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementTerminatedWorkersDoNotBlockCompletion(t *testing.T) {
	for _, liveWorker := range []bool{false, true} {
		name := "terminated only"
		if liveWorker {
			name = "terminated and live"
		}
		t.Run(name, func(t *testing.T) {
			x := activationCoordinatorFixture(t)
			ctx := context.Background()
			worker, found, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			worker.IsTerminated = true
			if err := x.f.store.UpdateSession(ctx, worker); err != nil {
				t.Fatal(err)
			}
			var live domain.SessionRecord
			if liveWorker {
				live, err = x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: x.operation.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()})
				if err != nil {
					t.Fatal(err)
				}
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil || op.WriterActivation == nil {
				t.Fatal("activation failed", op.Error, err)
			}
			if liveWorker {
				if len(op.Notifications) != 1 || op.Notifications[0].SessionID != live.ID {
					t.Fatal("notification targets must contain only the live worker", op.Notifications)
				}
			} else if len(op.Notifications) != 0 {
				t.Fatal("terminated worker must not require notification", op.Notifications)
			}
			for range 3 {
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if op.IntakeReleasedAt == nil || op.IntakeDrainRequestedAt == nil {
				t.Fatal("intake was not released and drained", op)
			}
			if !liveWorker && op.Phase != domain.ReplacementCompleted {
				t.Fatal("terminated worker blocked completion", op.Phase, op.Error)
			}
			if liveWorker && op.Phase != domain.ReplacementNotifying {
				t.Fatal("live notification was lost", op.Phase)
			}
			if _, found, err := x.f.store.GetActiveOrchestratorReplacement(ctx, op.ProjectID); err != nil || found != liveWorker {
				t.Fatal("active reservation does not match pending notifications", found, err)
			}
			if stored, found, err := x.f.store.GetSession(ctx, worker.ID); err != nil || !found || !stored.IsTerminated {
				t.Fatal("worker history changed", found, err)
			}
		})
	}
}
