package orchestratorreplacement_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementRetainedRetryWithoutFactsCannotExhaustRecoveryReserve(t *testing.T) {
	x := bootstrapSettlementFixture(t, false)
	ctx := context.Background()
	x.f.runtime.successorObservation.ControlTurn = nil
	x.f.runtime.successorObservationErr = errors.New("temporary native observation unavailable")
	exhaustOrdinaryRepairHistory(t, x)
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil || op.Phase != domain.ReplacementRecoveryRequired {
		t.Fatal(op.Phase, err)
	}
	op, err = x.service().Retry(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision := op.Revision
	for range 128 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal("unchanged observation exhausted reserve", err)
		}
		op, err = x.service().Retry(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if op.Revision != revision || op.Phase != domain.ReplacementControllerReady {
			t.Fatal("no-fact retry loop consumed checkpoint", revision, op.Revision, op.Phase)
		}
	}
	// Same authorization remains operable when positive main completion arrives.
	x.f.runtime.successorObservationErr = nil
	x.f.runtime.successorObservation.ControlTurn = &domain.ConversationTurn{ID: op.Bootstrap.TurnID, HandledBySessionID: op.SuccessorSessionID, State: domain.TurnStateCompleted, ProviderTurnID: "recovered-bootstrap-native"}
	op, err = x.service().Advance(ctx, op.ID)
	if err != nil || op.Phase != domain.ReplacementHandoffSettled {
		t.Fatal("reserved completion stranded", op.Phase, op.Error, err)
	}
}
