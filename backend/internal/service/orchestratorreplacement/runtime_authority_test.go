package orchestratorreplacement_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementRetirementCannotBeMintedByGenericSave(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementTranscriptVerified })
	ctx := context.Background()
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil || op.Phase != domain.ReplacementRetiring {
		t.Fatal(op.Phase, err)
	}
	now := time.Now().UTC()
	op.SourceRetiredAt = &now
	op.Phase = domain.ReplacementStarting
	if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
		t.Fatalf("generic save minted retirement: saved=%t err=%v", saved, err)
	}
	source, _, err := x.f.store.GetSession(ctx, op.SourceSessionID)
	if err != nil || source.IsTerminated {
		t.Fatal("source changed", err)
	}
}

func TestReplacementRuntimeReceiptsCannotBeMintedByGenericSave(t *testing.T) {
	for _, kind := range []string{"source stop", "bootstrap", "resume"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var x *preparedExport
			switch kind {
			case "bootstrap":
				x = bootstrapCoordinatorFixture(t)
			case "resume":
				x = settledBootstrapCoordinatorFixture(t)
			default:
				x = preparedExportFixture(t)
				x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.SourceStopIntentAt != nil })
			}
			op := x.operation
			now := time.Now().UTC()
			switch kind {
			case "source stop":
				op.SourceStop = &domain.ReplacementControllerStop{Owner: op.SourceOwner, CapabilityVerifier: op.SourceCapabilityVerifier, NativeID: op.SourceOwner.ProviderConversationID, Method: "chat-hibernate", ConfirmedAt: now}
			case "bootstrap":
				op.Bootstrap = &domain.ReplacementBootstrap{PromptHash: strings.Repeat("a", 64), IntentAt: now, TurnID: "forged-main-completion", SettledAt: &now}
			case "resume":
				p := op.SuccessorController
				op.Phase = domain.ReplacementVerifyingResume
				op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: now, BeforeOwner: p.Owner, BeforeCapabilityVerifier: p.CapabilityVerifier, BeforeNativeID: p.Owner.ProviderConversationID}
			}
			if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
				t.Fatalf("generic save minted %s: saved=%t err=%v", kind, saved, err)
			}
		})
	}
}
