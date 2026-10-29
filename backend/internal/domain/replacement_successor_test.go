package domain_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementExpectedSuccessorFollowsOnlyVerifiedResumeReceipt(t *testing.T) {
	for _, stage := range []string{"initial", "resumed", "recovery", "missing stop", "unverified history", "different native", "wrong generation", "wrong verifier", "no source retirement"} {
		t.Run(stage, func(t *testing.T) {
			now := time.Now().UTC()
			before := domain.SessionControllerOwner{Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, ControllerGeneration: "before", ProviderConversationID: "same-native"}
			after := before
			after.ControllerGeneration = "after"
			op := domain.OrchestratorReplacement{Phase: domain.ReplacementVerifyingResume, SourceRetiredAt: &now, SuccessorSessionID: "successor", SuccessorController: &domain.ReplacementSuccessorController{Owner: before, CapabilityVerifier: "before-verifier", CommittedAt: now}}
			if stage != "initial" {
				op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: now, BeforeOwner: before, BeforeNativeID: "same-native", BeforeCapabilityVerifier: "before-verifier", StopConfirmed: true, StopReceipt: &domain.ReplacementControllerStop{Owner: before, NativeID: "same-native", CapabilityVerifier: "before-verifier", Method: "chat-hibernate", ConfirmedAt: now}, ResumeIntentAt: &now, ResumeGeneration: "after", ResumedAt: &now, AfterOwner: after, AfterNativeID: "same-native", AfterCapabilityVerifier: "after-verifier", HistoryVerified: true}
			}
			switch stage {
			case "recovery":
				op.Phase = domain.ReplacementRecoveryRequired
			case "missing stop":
				op.ResumeEvidence.StopConfirmed = false
			case "unverified history":
				op.ResumeEvidence.HistoryVerified = false
			case "different native":
				op.ResumeEvidence.AfterNativeID = "different-native"
			case "wrong generation":
				op.ResumeEvidence.AfterOwner.ControllerGeneration = "unexpected"
			case "wrong verifier":
				op.ResumeEvidence.AfterCapabilityVerifier = "before-verifier"
			case "no source retirement":
				op.SourceRetiredAt = nil
			}
			proof, ok := op.ExpectedSuccessorController()
			if stage == "initial" || stage == "resumed" {
				want := before
				if stage == "resumed" {
					want = after
				}
				if !ok || proof.Owner != want || proof.CapabilityVerifier == "" || proof.CommittedAt.IsZero() {
					t.Fatal("wrong expected controller", stage, proof, ok)
				}
			} else if ok {
				t.Fatal("unverified resume selected a writer/probe owner", stage, proof)
			}
		})
	}
}
