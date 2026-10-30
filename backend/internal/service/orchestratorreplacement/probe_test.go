package orchestratorreplacement_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

func resumedSuccessorCoordinatorFixture(t *testing.T) *preparedExport {
	return nativeResumeFixture(t, true)
}

func nativeResumeFixture(t *testing.T, resume bool) *preparedExport {
	t.Helper()
	x := stoppedSuccessorCoordinatorFixture(t)
	ctx := context.Background()
	x.f.runtime.nativeResume = func(ctx context.Context, op domain.OrchestratorReplacement, allowNew bool) (ports.ReplacementResumeObservation, error) {
		if !allowNew {
			t.Fatal("fixture unexpectedly tried lookup-only resume")
		}
		rec, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
		if err != nil {
			t.Fatal(err)
		}
		rec.Metadata.ControllerGeneration, rec.Metadata.BrowserCapabilityVerifier = op.ResumeEvidence.ResumeGeneration, "after-verifier"
		if err := x.f.store.UpdateSession(ctx, rec); err != nil {
			t.Fatal(err)
		}
		rec, _, err = x.f.store.GetSession(ctx, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if applied, err := x.f.store.SetSessionHibernated(ctx, rec.ID, rec.Revision, nil); err != nil || !applied {
			t.Fatal(applied, err)
		}
		return ports.ReplacementResumeObservation{Owner: rec.ControllerOwner(), NativeID: rec.Metadata.ProviderConversationID, CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, HistoryVerified: true, ObservedAt: time.Now().UTC()}, nil
	}
	var err error
	if !resume {
		return x
	}
	x.operation, err = x.service().Advance(ctx, x.operation.ID)
	if err != nil || x.operation.ResumeEvidence.ResumedAt == nil {
		t.Fatal(x.operation.Phase, x.operation.Error, err)
	}
	p := x.operation.ResumeEvidence
	x.f.runtime.successorObservation = replacement.SourceObservation{Owner: p.AfterOwner, NativeID: p.AfterNativeID, Provenance: domain.NativeThreadMain, State: ports.ChatControllerReady, ObservedAt: time.Now().UTC()}
	return x
}

func TestReplacementContinuityProbeRequiresAcknowledgedMainCompletion(t *testing.T) {
	for _, result := range []string{"acknowledged", "wrong ack", "running", "failed", "approval", "wrong turn", "unknown provenance", "uncertain", "crash before send", "response lost"} {
		t.Run(result, func(t *testing.T) {
			x := resumedSuccessorCoordinatorFixture(t)
			ctx := context.Background()
			sends := 0
			x.f.runtime.probe = func(ctx context.Context, op domain.OrchestratorReplacement, prompt string, allowNew bool) (replacement.PreparationDelivery, error) {
				stored, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
				if err != nil || stored.ResumeEvidence.Probe == nil {
					t.Fatal("probe preceded durable intent", err)
				}
				probe := stored.ResumeEvidence.Probe
				sum := sha256.Sum256([]byte(prompt))
				ack, ok := domain.ReplacementProbeAcknowledgment(stored)
				if probe.IntentAt.IsZero() || probe.PromptHash != hex.EncodeToString(sum[:]) || !ok || !strings.Contains(prompt, ack) {
					t.Fatal("probe prompt not bound to nonce/intent")
				}
				if !allowNew || result == "uncertain" {
					return replacement.PreparationDelivery{Uncertain: true}, nil
				}
				if result == "crash before send" {
					panic("lost probe caller before send")
				}
				sends++
				if result == "response lost" {
					return replacement.PreparationDelivery{}, errors.New("probe receipt lost")
				}
				return replacement.PreparationDelivery{TurnID: "probe-turn", Owner: op.ResumeEvidence.AfterOwner, NativeID: op.ResumeEvidence.AfterNativeID}, nil
			}
			if result == "crash before send" {
				var crash any
				func() { defer func() { crash = recover() }(); _, _ = x.service().Advance(ctx, x.operation.ID) }()
				if crash != "lost probe caller before send" {
					t.Fatal("wrong probe crash boundary", crash)
				}
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if result == "uncertain" || result == "crash before send" || result == "response lost" {
				if op.Phase != domain.ReplacementRecoveryRequired || op.ResumeEvidence.Probe == nil || op.ResumeEvidence.VerifiedAt != nil || sends > 1 {
					t.Fatal("uncertain probe replayed/verified", op.Phase, sends)
				}
				return
			}
			if op.Phase != domain.ReplacementVerifyingResume || op.ResumeEvidence.Probe == nil || op.ResumeEvidence.Probe.TurnID != "probe-turn" || op.ResumeEvidence.VerifiedAt != nil || sends != 1 {
				t.Fatal("probe acceptance claimed completion", op.Phase, sends)
			}
			turn := &domain.ConversationTurn{ID: "probe-turn", HandledBySessionID: op.SuccessorSessionID, ProviderTurnID: "probe-native-turn", State: domain.TurnStateCompleted}
			x.f.runtime.successorObservation.ControlAcknowledged = true
			switch result {
			case "wrong ack":
				x.f.runtime.successorObservation.ControlAcknowledged = false
			case "running":
				turn.State = domain.TurnStateRunning
			case "failed":
				turn.State = domain.TurnStateFailed
			case "approval":
				x.f.runtime.successorObservation.PendingApproval = true
			case "wrong turn":
				turn.ID = "another-turn"
			case "unknown provenance":
				x.f.runtime.successorObservation.Provenance = domain.NativeThreadUnknown
			}
			x.f.runtime.successorObservation.ControlTurn = turn
			op, err = x.service().Advance(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch result {
			case "acknowledged":
				if op.Phase != domain.ReplacementActivatingSharedWriter || op.ResumeEvidence.Probe.SettledAt == nil || op.ResumeEvidence.VerifiedAt == nil {
					t.Fatal("main nonce probe not verified", op.Phase, op.Error)
				}
				for _, mutation := range []string{"nonce", "prompt", "intent", "turn", "settled", "clear verification"} {
					stored, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
					if err != nil {
						t.Fatal(err)
					}
					proof := stored.ResumeEvidence
					switch mutation {
					case "nonce":
						proof.Probe.Nonce = "55555555-5555-4555-8555-555555555555"
					case "prompt":
						proof.Probe.PromptHash = strings.Repeat("e", 64)
					case "intent":
						proof.Probe.IntentAt = proof.Probe.IntentAt.Add(time.Second)
					case "turn":
						proof.Probe.TurnID = "another-turn"
					case "settled":
						later := proof.Probe.SettledAt.Add(time.Second)
						proof.Probe.SettledAt = &later
						proof.VerifiedAt = &later
					case "clear verification":
						proof.Probe.SettledAt = nil
						proof.VerifiedAt = nil
					}
					if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, stored); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
						t.Fatal("continuity receipt rewritten", mutation, saved, err)
					}
				}
			case "running", "approval":
				if op.Phase != domain.ReplacementVerifyingResume || op.ResumeEvidence.VerifiedAt != nil {
					t.Fatal("unsettled probe advanced", op.Phase)
				}
			default:
				if op.Phase != domain.ReplacementRecoveryRequired || op.ResumeEvidence.VerifiedAt != nil {
					t.Fatal("unverified probe advanced", result, op.Phase)
				}
			}
			state, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil || state.Lease == nil || !state.Lease.Frozen || state.Lease.SessionID != op.SourceSessionID {
				t.Fatal("probe activated writer early", err)
			}
			if sends != 1 || x.f.runtime.nativeResumes != 1 || x.f.runtime.successorStops != 1 {
				t.Fatal("probe replayed earlier effects", sends)
			}
		})
	}
}
