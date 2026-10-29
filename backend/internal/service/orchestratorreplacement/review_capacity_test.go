package orchestratorreplacement_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

func busySource(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
	return domain.ReplacementControllerStop{}, domain.ErrReplacementSourceBusy
}

// Fill with metadata-only saves, never with provider or author facts. A finite
// ordinary budget must refuse more noise while leaving recovery writes usable.
func exhaustOrdinaryRepairHistory(t *testing.T, x *preparedExport) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 1200; i++ {
		op, found, err := x.f.store.GetOrchestratorReplacement(ctx, x.operation.ID)
		if err != nil || !found {
			t.Fatal(found, err)
		}
		op.Error = &domain.ReplacementFailure{Code: "QUOTA_FIXTURE", Message: fmt.Sprint(i)}
		applied, err := x.f.store.SaveOrchestratorReplacement(ctx, op)
		if errors.Is(err, domain.ErrReplacementRepairCapacity) {
			x.operation, _, err = x.f.store.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if x.operation.Revision != 1024 {
				t.Fatalf("metadata spent recovery credits: revision=%d", x.operation.Revision)
			}
			return
		}
		if err != nil || !applied {
			t.Fatal(applied, err)
		}
	}
	t.Fatal("metadata-only saves consumed unbounded checkpoint capacity")
}

func TestReplacementCapacityRefusesNewStopIntentAndStillCancels(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementExportingTranscript })
	exhaustOrdinaryRepairHistory(t, x)
	before := x.f.runtime.quiescences
	svc := x.service()
	_, err := svc.Advance(context.Background(), x.operation.ID)
	var api *apierr.Error
	if !errors.As(err, &api) || api.Code != "ORCHESTRATOR_REPLACEMENT_REPAIR_CAPACITY" {
		t.Errorf("capacity admission was not a named refusal: %v", err)
	}
	stored, _, err := x.f.store.GetOrchestratorReplacement(context.Background(), x.operation.ID)
	if err != nil || stored.SourceStopIntentAt != nil || stored.SourceStop != nil || x.f.runtime.quiescences != before {
		t.Fatalf("capacity refusal attempted provider work: intent=%v stop=%v calls=%d err=%v", stored.SourceStopIntentAt, stored.SourceStop, x.f.runtime.quiescences, err)
	}
	if _, err := svc.Cancel(context.Background(), x.operation.ID); err != nil {
		t.Fatal("capacity refusal trapped a running source", err)
	}
	state, err := x.f.store.GetProjectDocumentState(context.Background(), stored.ProjectID)
	if err != nil || state.Lease.Frozen {
		t.Fatal("cancel did not release the writer", err)
	}
}

func TestReplacementCapacityStillClearsBusyIntentAndCancels(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.SourceStopIntentAt != nil })
	exhaustOrdinaryRepairHistory(t, x)
	x.f.runtime.quiesce = busySource
	svc := expiredExportService(x)
	op, err := svc.Advance(context.Background(), x.operation.ID)
	if err != nil || op.SourceStopIntentAt != nil || !op.CanReleaseSourceIntake() {
		t.Fatalf("ordinary capacity trapped positive no-stop clearance: phase=%s intent=%v err=%v", op.Phase, op.SourceStopIntentAt, err)
	}
	if _, err := svc.Cancel(context.Background(), op.ID); err != nil {
		t.Fatal("cleared intent could not cancel", err)
	}
	if *x.actualStops != 0 {
		t.Fatal("busy source was stopped")
	}
}

func TestReplacementCapacityStillRecordsStopAndPublishesEvidence(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.SourceStopIntentAt != nil })
	exhaustOrdinaryRepairHistory(t, x)
	op, err := x.service().Advance(context.Background(), x.operation.ID)
	if err != nil || op.SourceStop == nil || *x.actualStops != 1 {
		t.Fatalf("provider stopped without a durable receipt: stop=%v actualStops=%d err=%v", op.SourceStop, *x.actualStops, err)
	}
	x.operation = op
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementTranscriptVerified })
	if x.operation.Transcript.Receipt == nil || *x.actualStops != 1 {
		t.Fatal("capacity lost evidence or repeated the provider stop")
	}
}
