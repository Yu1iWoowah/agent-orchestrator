package orchestratorreplacement_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

// Audit 78 I4, including failed turns that were already delivered to provider.
func TestReplacementFailedControlTurnRetryUsesNewAttempt(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		for _, kind := range []string{"bootstrap", "probe"} {
			name := kind + "/not dispatched"
			if delivered {
				name = kind + "/provider failed"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				var x *preparedExport
				if kind == "bootstrap" {
					x = bootstrapCoordinatorFixture(t)
				} else {
					x = resumedSuccessorCoordinatorFixture(t)
				}
				deliver := func(_ context.Context, op domain.OrchestratorReplacement, _ string, _ bool) (replacement.PreparationDelivery, error) {
					proof, _ := op.ExpectedSuccessorController()
					return replacement.PreparationDelivery{TurnID: "failed-" + kind, Owner: proof.Owner, NativeID: proof.Owner.ProviderConversationID}, nil
				}
				if kind == "bootstrap" {
					x.f.runtime.bootstrap = deliver
				} else {
					x.f.runtime.probe = deliver
				}
				op, err := x.service().Advance(ctx, x.operation.ID)
				if err != nil {
					t.Fatal(err)
				}
				oldID := op.BootstrapClientID
				oldNonce := ""
				if kind == "probe" {
					oldID = op.ResumeProbeClientID
					oldNonce = op.ResumeEvidence.Probe.Nonce
				}
				x.f.runtime.successorObservation.ControlTurn = &domain.ConversationTurn{ID: "failed-" + kind, HandledBySessionID: op.SuccessorSessionID, State: domain.TurnStateFailed}
				if delivered {
					x.f.runtime.successorObservation.ControlTurn.ProviderTurnID = "failed-native-" + kind
				}
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil || op.Phase != domain.ReplacementRecoveryRequired {
					t.Fatal(op.Phase, op.Error, err)
				}
				op, err = x.service().Retry(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				newID := op.BootstrapClientID
				if kind == "probe" {
					newID = op.ResumeProbeClientID
				}
				if oldID == newID {
					t.Fatal("retry is still deduplicated to failed control turn", newID)
				}
				// Every old receipt remains in durable history, and generic save cannot erase it.
				if len(op.ControlAttempts) != 1 {
					t.Fatal("failed receipt history missing")
				}
				for _, save := range []func(context.Context, domain.OrchestratorReplacement) (bool, error){x.f.store.SaveOrchestratorReplacement, x.f.store.SaveReplacementRuntimeProgress} {
					forged := op
					forged.ControlAttempts = nil
					if saved, err := save(ctx, forged); saved || err == nil {
						t.Fatal("failed control history could be erased", saved, err)
					}
				}
				x.f.runtime.successorObservation.ControlTurn = nil
				accepted := func(_ context.Context, op domain.OrchestratorReplacement, _ string, _ bool) (replacement.PreparationDelivery, error) {
					proof, _ := op.ExpectedSuccessorController()
					return replacement.PreparationDelivery{TurnID: "retry-" + kind, Owner: proof.Owner, NativeID: proof.Owner.ProviderConversationID}, nil
				}
				if kind == "bootstrap" {
					x.f.runtime.bootstrap = accepted
				} else {
					x.f.runtime.probe = accepted
				}
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "probe" && op.ResumeEvidence.Probe.Nonce == oldNonce {
					t.Fatal("probe nonce reused")
				}
				x.f.runtime.successorObservation.ControlTurn = &domain.ConversationTurn{ID: "retry-" + kind, HandledBySessionID: op.SuccessorSessionID, State: domain.TurnStateCompleted, ProviderTurnID: "retry-native-" + kind}
				x.f.runtime.successorObservation.State = ports.ChatControllerReady
				x.f.runtime.successorObservation.ControlAcknowledged = true
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				phase := domain.ReplacementHandoffSettled
				if kind == "probe" {
					phase = domain.ReplacementActivatingSharedWriter
				}
				if op.Phase != phase {
					t.Fatal("new control attempt did not settle", op.Phase, op.Error)
				}
			})
		}
	}
}
