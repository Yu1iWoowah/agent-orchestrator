package orchestratorreplacement_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

func settledBootstrapCoordinatorFixture(t *testing.T) *preparedExport {
	return bootstrapSettlementFixture(t, true)
}

func bootstrapSettlementFixture(t *testing.T, settle bool) *preparedExport {
	t.Helper()
	x := bootstrapCoordinatorFixture(t)
	ctx := context.Background()
	x.f.runtime.bootstrap = func(_ context.Context, op domain.OrchestratorReplacement, _ string, _ bool) (replacement.PreparationDelivery, error) {
		return replacement.PreparationDelivery{TurnID: "bootstrap-turn", Owner: op.SuccessorController.Owner, NativeID: op.SuccessorController.Owner.ProviderConversationID}, nil
	}
	var err error
	x.operation, err = x.service().Advance(ctx, x.operation.ID)
	if err != nil || x.operation.Bootstrap == nil {
		t.Fatal(err)
	}
	x.f.runtime.successorObservation.ControlTurn = &domain.ConversationTurn{ID: x.operation.Bootstrap.TurnID, HandledBySessionID: x.operation.SuccessorSessionID, State: domain.TurnStateCompleted, ProviderTurnID: "bootstrap-native"}
	if !settle {
		return x
	}
	x.operation, err = x.service().Advance(ctx, x.operation.ID)
	if err != nil || x.operation.Phase != domain.ReplacementHandoffSettled {
		t.Fatal(x.operation.Phase, err)
	}
	return x
}

func stoppedSuccessorCoordinatorFixture(t *testing.T) *preparedExport {
	return successorStopFixture(t, true)
}

func successorStopFixture(t *testing.T, stop bool) *preparedExport {
	t.Helper()
	x := settledBootstrapCoordinatorFixture(t)
	ctx := context.Background()
	x.f.runtime.successorStop = func(ctx context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
		rec, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if applied, err := x.f.store.SetReplacementSessionHibernated(ctx, op.ID, op.Revision, rec.ID, rec.Revision, now); err != nil || !applied {
			t.Fatal(applied, err)
		}
		return domain.ReplacementControllerStop{Owner: rec.ControllerOwner(), NativeID: rec.Metadata.ProviderConversationID, CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, Method: "chat-hibernate", ConfirmedAt: now}, nil
	}
	var err error
	x.operation, err = x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stop {
		return x
	}
	x.operation, err = x.service().Advance(ctx, x.operation.ID)
	if err != nil || x.operation.ResumeEvidence == nil || !x.operation.ResumeEvidence.StopConfirmed {
		t.Fatal(x.operation.Phase, err)
	}
	return x
}

func TestReplacementNativeResumePersistsExactAfterOwnerAndFailsClosed(t *testing.T) {
	for _, result := range []string{"resumed", "wrong native", "wrong generation", "wrong verifier", "history unverified", "no timestamp", "response lost", "crash before resume", "crash after resume"} {
		t.Run(result, func(t *testing.T) {
			x := stoppedSuccessorCoordinatorFixture(t)
			ctx := context.Background()
			actualResumes := 0
			x.f.runtime.nativeResume = func(ctx context.Context, op domain.OrchestratorReplacement, allowNew bool) (ports.ReplacementResumeObservation, error) {
				stored, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
				p := stored.ResumeEvidence
				if err != nil || p.ResumeIntentAt == nil || p.ResumeIntentAt.IsZero() || p.ResumeGeneration == "" || p.ResumeGeneration == p.BeforeOwner.ControllerGeneration || p.ResumeGeneration != op.ResumeEvidence.ResumeGeneration {
					t.Fatal("native resume preceded durable exact intent", err)
				}
				if !allowNew {
					return ports.ReplacementResumeObservation{}, ports.ErrChatRecoveryInconclusive
				}
				if result == "crash before resume" {
					panic("lost caller before native resume")
				}
				actualResumes++
				rec, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
				if err != nil {
					t.Fatal(err)
				}
				rec.Metadata.ControllerGeneration, rec.Metadata.BrowserCapabilityVerifier = p.ResumeGeneration, "after-verifier"
				rec.HibernatedAt = nil
				if result == "wrong native" {
					rec.Metadata.ProviderConversationID = "fresh-native"
				}
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
				rec, _, err = x.f.store.GetSession(ctx, rec.ID)
				if err != nil {
					t.Fatal(err)
				}
				if result == "crash after resume" {
					panic("lost caller after native resume")
				}
				if result == "response lost" {
					return ports.ReplacementResumeObservation{}, errors.New("native resume response lost")
				}
				observed := ports.ReplacementResumeObservation{Owner: rec.ControllerOwner(), NativeID: rec.Metadata.ProviderConversationID, CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, HistoryVerified: true, ObservedAt: time.Now().UTC()}
				switch result {
				case "wrong generation":
					observed.Owner.ControllerGeneration = "another-generation"
				case "wrong verifier":
					observed.CapabilityVerifier = "different-verifier"
				case "history unverified":
					observed.HistoryVerified = false
				case "no timestamp":
					observed.ObservedAt = time.Time{}
				}
				return observed, nil
			}
			if result == "crash before resume" || result == "crash after resume" {
				var crash any
				func() { defer func() { crash = recover() }(); _, _ = x.service().Advance(ctx, x.operation.ID) }()
				if crash == nil {
					t.Fatal("native resume crash boundary not reached")
				}
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if result == "resumed" {
				p := op.ResumeEvidence
				if op.Phase != domain.ReplacementVerifyingResume || p.ResumedAt == nil || !p.HistoryVerified || p.AfterNativeID != p.BeforeNativeID || p.AfterOwner.ControllerGeneration != p.ResumeGeneration || p.AfterCapabilityVerifier != "after-verifier" || p.VerifiedAt != nil {
					t.Fatal("after receipt omitted or claimed final probe", op.Phase, op.Error)
				}
				for _, mutation := range []string{"intent", "generation", "owner", "verifier", "resumed", "clear after"} {
					stored, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
					if err != nil {
						t.Fatal(err)
					}
					proof := stored.ResumeEvidence
					switch mutation {
					case "intent":
						later := proof.ResumeIntentAt.Add(time.Second)
						proof.ResumeIntentAt = &later
					case "generation":
						proof.ResumeGeneration = "different-reservation"
						proof.AfterOwner.ControllerGeneration = proof.ResumeGeneration
					case "owner":
						proof.AfterOwner.RuntimeLaunchID = "different-launch"
					case "verifier":
						proof.AfterCapabilityVerifier = "different-verifier"
					case "resumed":
						later := proof.ResumedAt.Add(time.Second)
						proof.ResumedAt = &later
					case "clear after":
						proof.ResumedAt = nil
						proof.HistoryVerified = false
						proof.AfterOwner = domain.SessionControllerOwner{}
						proof.AfterNativeID = ""
						proof.AfterCapabilityVerifier = ""
					}
					if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, stored); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
						t.Fatal("native resume receipt rewritten", mutation, saved, err)
					}
				}
			} else if op.Phase != domain.ReplacementRecoveryRequired || op.ResumeEvidence.ResumedAt != nil || op.ResumeEvidence.HistoryVerified {
				t.Fatal("unverified native resume accepted", result, op.Phase, op.Error)
			}
			wantResumes := 1
			if result == "crash before resume" {
				wantResumes = 0
			}
			if actualResumes != wantResumes || op.SuccessorSessionID != x.operation.SuccessorSessionID || x.f.runtime.successorStops != 1 {
				t.Fatal("resume replayed shutdown/identity", actualResumes, x.f.runtime.successorStops)
			}
		})
	}
}

func TestReplacementResumeStopIntentAndReceiptSurviveCallerLoss(t *testing.T) {
	for _, loss := range []string{"none", "after stop", "wrong receipt"} {
		t.Run(loss, func(t *testing.T) {
			x := settledBootstrapCoordinatorFixture(t)
			ctx := context.Background()
			actualStops := 0
			x.f.runtime.successorStop = func(ctx context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
				stored, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
				if err != nil || stored.ResumeEvidence == nil || stored.ResumeEvidence.StartedAt.IsZero() || stored.ResumeEvidence.BeforeOwner != op.SuccessorController.Owner || stored.ResumeEvidence.StopConfirmed {
					t.Fatal("stop preceded bound durable intent", err)
				}
				if actualStops > 0 {
					return domain.ReplacementControllerStop{}, ports.ErrChatRecoveryInconclusive
				}
				rec, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				if applied, err := x.f.store.SetReplacementSessionHibernated(ctx, op.ID, op.Revision, rec.ID, rec.Revision, now); err != nil || !applied {
					t.Fatal(applied, err)
				}
				actualStops++
				if loss == "after stop" {
					panic("lost successor stop receipt")
				}
				stop := domain.ReplacementControllerStop{Owner: rec.ControllerOwner(), CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, NativeID: rec.Metadata.ProviderConversationID, Method: "chat-hibernate", ConfirmedAt: now}
				if loss == "wrong receipt" {
					stop.NativeID = "different-native"
				}
				return stop, nil
			}
			intent, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil || intent.Phase != domain.ReplacementVerifyingResume || intent.ResumeEvidence == nil || intent.ResumeEvidence.StartedAt.IsZero() || intent.ResumeEvidence.StopConfirmed || x.f.runtime.successorStops != 0 {
				t.Fatal("missing independent stop intent", intent.Phase, err)
			}
			if loss == "after stop" {
				var crash any
				func() { defer func() { crash = recover() }(); _, _ = x.service().Advance(ctx, intent.ID) }()
				if crash != "lost successor stop receipt" {
					t.Fatal("wrong stop crash", crash)
				}
			}
			result, err := x.service().Advance(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loss == "none" {
				if result.Phase != domain.ReplacementVerifyingResume || !result.ResumeEvidence.StopConfirmed || result.ResumeEvidence.StopReceipt == nil || result.ResumeEvidence.HistoryVerified || result.ResumeEvidence.AfterNativeID != "" {
					t.Fatal("stop receipt claimed resume", result.Phase, result.Error)
				}
			} else if result.Phase != domain.ReplacementRecoveryRequired || result.ResumeEvidence.StopConfirmed || result.ResumeEvidence.StopReceipt != nil {
				t.Fatal("uncertain stop manufactured receipt", result.Phase, result.Error)
			}
			if actualStops != 1 || result.SuccessorSessionID != intent.SuccessorSessionID || result.SourceRetiredAt == nil {
				t.Fatal("stop replayed or source revived", actualStops)
			}
		})
	}
}
