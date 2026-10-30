package chat_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func settledSuccessorFixture(t *testing.T) (successorChatFixture, *hibernationConversation) {
	t.Helper()
	var provider *hibernationConversation
	f := successorChatReadyFixtureWithProvider(t, func(native *fakeConversation) ports.ChatConversation {
		provider = &hibernationConversation{fakeConversation: native}
		return provider
	})
	f, prompt := successorBootstrapIntentFor(t, f)
	ctx := context.Background()
	receipt, err := f.service.EnsureReplacementChatBootstrap(ctx, f.op, prompt, true)
	if err != nil {
		t.Fatal(err)
	}
	provider.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1", TurnState: domain.TurnStateCompleted})
	waitForTurnState(t, f.service, f.target.ID, domain.TurnStateCompleted)
	conversation, err := f.store.ConversationForSession(ctx, f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"providerTurnId":%q,"providerConversationId":%q}`, "provider-turn-1", f.op.SuccessorController.Owner.ProviderConversationID)
	if _, err := f.store.ProjectProviderEvent(ctx, conversation.ID, f.target.ID, f.op.SuccessorController.Owner.ControllerGeneration, "bootstrap-identity", "turn.started", payload, time.Now(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f.op.Bootstrap.TurnID, f.op.Bootstrap.SettledAt = receipt.TurnID, &now
	f.op.Phase = domain.ReplacementHandoffSettled
	if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	f.op, _, err = f.store.GetOrchestratorReplacement(ctx, f.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, provider
}

// The completed DB row can precede release of the event/drain dispatch lock.
// Only retry the explicit no-stop busy result; uncertainty is never retried.
func quiesceIdleSuccessor(t *testing.T, f successorChatFixture) domain.ReplacementControllerStop {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stop, err := f.service.QuiesceReplacementChatSuccessor(context.Background(), f.op)
		if err == nil {
			return stop
		}
		if !errors.Is(err, domain.ErrReplacementSourceBusy) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReplacementSuccessorStopRequiresBoundSettledController(t *testing.T) {
	for _, change := range []string{"idle", "background", "running", "capability", "changed operation", "missing bootstrap", "wrong before owner", "shutdown unknown", "no live controller"} {
		t.Run(change, func(t *testing.T) {
			f, provider := settledSuccessorFixture(t)
			ctx := context.Background()
			f.op.Phase = domain.ReplacementVerifyingResume
			f.op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: time.Now().UTC(), BeforeOwner: f.op.SuccessorController.Owner, BeforeNativeID: f.op.SuccessorController.Owner.ProviderConversationID, BeforeCapabilityVerifier: f.op.SuccessorController.CapabilityVerifier}
			if change == "wrong before owner" {
				f.op.ResumeEvidence.BeforeOwner.ControllerGeneration = "different"
			}
			if change == "missing bootstrap" {
				f.op.Bootstrap.SettledAt = nil
			}
			if change == "missing bootstrap" || change == "wrong before owner" {
				// Clearing a settlement is itself forbidden by durable receipt fencing.
				if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
					t.Fatal("settlement could be cleared", saved, err)
				}
				return
			}
			if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			var err error
			f.op, _, err = f.store.GetOrchestratorReplacement(ctx, f.op.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "background":
				provider.backgroundRunning = true
			case "running":
				// Use durable running-turn facts, rather than a display status or
				// ordinary input that the replacement queue correctly holds.
				if err := f.store.BindTurnToProvider(ctx, f.op.Bootstrap.TurnID, "another-running-turn", time.Now()); err != nil {
					t.Fatal(err)
				}
			case "capability":
				provider.onBackgroundCheck = func(context.Context) {
					rec, _, err := f.store.GetSession(ctx, f.target.ID)
					if err != nil {
						t.Fatal(err)
					}
					if applied, err := f.store.UpdateBrowserCapabilityVerifier(ctx, rec.ID, rec.ControllerOwner(), "rotated"); err != nil || !applied {
						t.Fatal(applied, err)
					}
				}
			case "changed operation":
				provider.onBackgroundCheck = func(context.Context) {
					f.op.Phase = domain.ReplacementRecoveryRequired
					if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); err != nil || !saved {
						t.Fatal(saved, err)
					}
				}
			case "shutdown unknown":
				provider.shutdownErr = errors.New("provider shutdown was not confirmed")
			case "no live controller":
				if err := f.service.Stop(ctx, f.target.ID); err != nil {
					t.Fatal(err)
				}
			}
			var stop domain.ReplacementControllerStop
			if change == "idle" {
				stop = quiesceIdleSuccessor(t, f)
			} else {
				stop, err = f.service.QuiesceReplacementChatSuccessor(ctx, f.op)
			}
			if change == "idle" {
				if err != nil || stop.Owner != f.op.SuccessorController.Owner || stop.CapabilityVerifier != f.op.SuccessorController.CapabilityVerifier || stop.NativeID != f.op.SuccessorController.Owner.ProviderConversationID || stop.ConfirmedAt.IsZero() || provider.calls.Load() != 1 || f.service.HasLiveChatController(f.target.ID) {
					t.Fatal("successor shutdown not strongly bound", stop, err)
				}
				if again, err := f.service.QuiesceReplacementChatSuccessor(ctx, f.op); err == nil || !again.ConfirmedAt.IsZero() || provider.calls.Load() != 1 {
					t.Fatal("missing controller manufactured another stop", again, err)
				}
			} else if err == nil || !stop.ConfirmedAt.IsZero() {
				t.Fatal("unverified stop produced receipt", change, stop, err)
			}
			if change != "idle" && change != "shutdown unknown" && provider.calls.Load() != 0 {
				t.Fatal("unsafe target stopped", change, provider.calls.Load())
			}
		})
	}
}

func TestReplacementResumeStopReceiptCannotBeRewritten(t *testing.T) {
	for _, change := range []string{"before owner", "before native", "before verifier", "started", "stop receipt", "clear confirmation"} {
		t.Run(change, func(t *testing.T) {
			f, _ := settledSuccessorFixture(t)
			ctx := context.Background()
			p := f.op.SuccessorController
			f.op.Phase = domain.ReplacementVerifyingResume
			f.op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: time.Now().UTC(), BeforeOwner: p.Owner, BeforeNativeID: p.Owner.ProviderConversationID, BeforeCapabilityVerifier: p.CapabilityVerifier}
			if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			var err error
			f.op, _, err = f.store.GetOrchestratorReplacement(ctx, f.op.ID)
			if err != nil {
				t.Fatal(err)
			}
			stop := quiesceIdleSuccessor(t, f)
			f.op.ResumeEvidence.StopConfirmed, f.op.ResumeEvidence.StopReceipt = true, &stop
			if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			f.op, _, err = f.store.GetOrchestratorReplacement(ctx, f.op.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "before owner":
				f.op.ResumeEvidence.BeforeOwner.ControllerGeneration = "different"
			case "before native":
				f.op.ResumeEvidence.BeforeNativeID = "different"
			case "before verifier":
				f.op.ResumeEvidence.BeforeCapabilityVerifier = "different"
			case "started":
				f.op.ResumeEvidence.StartedAt = f.op.ResumeEvidence.StartedAt.Add(time.Second)
			case "stop receipt":
				f.op.ResumeEvidence.StopReceipt.ConfirmedAt = f.op.ResumeEvidence.StopReceipt.ConfirmedAt.Add(time.Second)
			case "clear confirmation":
				f.op.ResumeEvidence.StopConfirmed = false
				f.op.ResumeEvidence.StopReceipt = nil
			}
			if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
				t.Fatal("resume shutdown proof rewritten", change, saved, err)
			}
		})
	}
}
