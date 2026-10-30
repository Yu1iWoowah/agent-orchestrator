package orchestratorreplacement_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

func TestReplacementExportBusyBackoffDoesNotRewriteIntent(t *testing.T) {
	x := preparedExportFixture(t)
	ctx := context.Background()
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementExportingTranscript })
	now := time.Now().UTC()
	svc := replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: x.reader, Now: func() time.Time { return now }})
	x.f.runtime.quiesce = func(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
		return domain.ReplacementControllerStop{}, domain.ErrReplacementSourceBusy
	}
	op, err := svc.Advance(ctx, x.operation.ID)
	if err != nil || op.SourceStopIntentAt == nil {
		t.Fatal(err)
	}
	op, err = svc.Advance(ctx, op.ID)
	if err != nil || op.SourceStopIntentAt != nil || op.Error == nil {
		t.Fatal(err)
	}
	for range 8 {
		next, err := svc.Advance(ctx, op.ID)
		if err != nil || next.Revision != op.Revision || next.SourceStopIntentAt != nil {
			t.Fatalf("busy polling wrote another intent: rev %d/%d err=%v", next.Revision, op.Revision, err)
		}
	}
	if x.f.runtime.quiescences != 1 {
		t.Fatal("busy polling called provider", x.f.runtime.quiescences)
	}
	now = now.Add(6 * time.Second)
	next, err := svc.Advance(ctx, op.ID)
	if err != nil || next.SourceStopIntentAt == nil {
		t.Fatal("backoff never retried", err)
	}
}

func TestReplacementPostStopPollingWithoutFactsDoesNotWrite(t *testing.T) {
	x := bootstrapCoordinatorFixture(t)
	ctx := context.Background()
	x.f.runtime.successorObservation.State = ports.ChatControllerBusy
	before := x.operation
	for range 8 {
		next, err := x.service().Advance(ctx, before.ID)
		if err != nil || next.Revision != before.Revision {
			t.Fatal("post-stop busy poll wrote snapshot", next.Revision, before.Revision, err)
		}
	}
	if x.f.runtime.bootstraps != 0 {
		t.Fatal("busy poll sent bootstrap")
	}
}

func TestReplacementCapacityMessagePreservesOriginalFailure(t *testing.T) {
	x := preparedExportFixture(t)
	ctx := context.Background()
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementExportingTranscript })
	exhaustOrdinaryRepairHistory(t, x)
	op, err := expiredExportService(x).Advance(ctx, x.operation.ID)
	if err != nil || op.Error == nil || op.Error.Code != "ORCHESTRATOR_REPLACEMENT_REPAIR_CAPACITY" || !strings.Contains(op.Error.Message, "Source did not become quiescent before the deadline") {
		t.Fatalf("capacity pause hid original cause: %+v %v", op.Error, err)
	}
}
