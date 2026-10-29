package orchestratorreplacement_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

type replacementReconciler interface{ Reconcile(context.Context) error }

func TestReplacementReconcileResumesPersistedOperation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op, err := f.svc.Accept(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	restarted := replacement.New(replacement.Deps{Store: f.store, Runtime: f.runtime, Documents: f.docs})
	runner, ok := any(restarted).(replacementReconciler)
	if !ok {
		t.Fatal("persisted replacement has no daemon reconciliation entry point")
	}
	if err := runner.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Get(ctx, op.ID)
	if err != nil || got.Phase != domain.ReplacementPreflight {
		t.Fatal("persisted acceptance did not progress", got.Phase, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := runner.Reconcile(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled reconcile did not stop", err)
	}
	after, err := restarted.Get(ctx, op.ID)
	if err != nil || after.Revision != got.Revision {
		t.Fatal("cancelled reconcile changed operation", err)
	}
}

type reconcileErrorStore struct {
	*testStore
	operations []domain.OrchestratorReplacement
	failedID   string
	failure    error
}

func (s *reconcileErrorStore) ListActiveOrchestratorReplacements(context.Context) ([]domain.OrchestratorReplacement, error) {
	return s.operations, nil
}
func (s *reconcileErrorStore) GetOrchestratorReplacement(ctx context.Context, id string) (domain.OrchestratorReplacement, bool, error) {
	if id == s.failedID {
		return domain.OrchestratorReplacement{}, false, s.failure
	}
	return s.testStore.GetOrchestratorReplacement(ctx, id)
}
func TestReplacementReconcileIsolatesOperationErrors(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op, err := f.svc.Accept(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("one operation read failed")
	store := &reconcileErrorStore{testStore: f.store, operations: []domain.OrchestratorReplacement{{ID: "unreadable"}, op}, failedID: "unreadable", failure: failure}
	svc := replacement.New(replacement.Deps{Store: store, Runtime: f.runtime, Documents: f.docs})
	runner, ok := any(svc).(replacementReconciler)
	if !ok {
		t.Fatal("no reconciliation entry point")
	}
	if err := runner.Reconcile(ctx); !errors.Is(err, failure) {
		t.Fatal("operation failure lost", err)
	}
	got, err := svc.Get(ctx, op.ID)
	if err != nil || got.Phase != domain.ReplacementPreflight {
		t.Fatal("one failed operation starved another", got.Phase, err)
	}
}

func TestReplacementReconcileDoesNotRetryWaitingOrTerminalOperations(t *testing.T) {
	for _, phase := range []domain.ReplacementPhase{domain.ReplacementAwaitingSource, domain.ReplacementAwaitingHandoff, domain.ReplacementAwaitingTranscript, domain.ReplacementRecoveryRequired, domain.ReplacementCancelled} {
		t.Run(string(phase), func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			op, err := f.svc.Accept(ctx, f.request)
			if err != nil {
				t.Fatal(err)
			}
			op.Phase = phase
			if saved, err := f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			before, _, err := f.store.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.svc.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := f.svc.Get(ctx, op.ID)
			if err != nil || got.Phase != phase || got.Revision != before.Revision || got.SourceIntakeReleasedAt != nil || f.runtime.observations != 0 || f.runtime.preparations != 0 || f.runtime.quiescences != 0 || f.runtime.successors != 0 {
				t.Fatal("waiting operation acted without explicit retry", err)
			}
			if err := f.svc.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			again, err := f.svc.Get(ctx, op.ID)
			if err != nil || again.Revision != got.Revision {
				t.Fatal("paused reconciliation changed facts", err)
			}
		})
	}
}
