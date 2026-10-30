package chat_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestReplacementProbeCannotReuseBootstrapBeforeNativeResume(t *testing.T) {
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
	before := len(f.provider.sentTexts())
	if receipt, err := f.service.EnsureReplacementChatProbe(ctx, f.op, f.provider.sentTexts()[0], true); err == nil || receipt.TurnID != "" || len(f.provider.sentTexts()) != before {
		t.Fatal("bootstrap receipt reused as a resume probe", receipt, err)
	}
}

func TestReplacementContinuityProbeRequiresMainNonceAcknowledgment(t *testing.T) {
	for _, reply := range []string{"main ack", "wrong ack", "child ack", "unknown ack"} {
		t.Run(reply, func(t *testing.T) {
			f := resumedSuccessorFixture(t)
			ctx := context.Background()
			f.op.ResumeEvidence.Probe = &domain.ReplacementContinuityProbe{Nonce: "44444444-4444-4444-8444-444444444444", IntentAt: time.Now().UTC()}
			ack, ok := domain.ReplacementProbeAcknowledgment(f.op)
			if !ok {
				t.Fatal("missing probe acknowledgment identity")
			}
			prompt := "AO continuity probe: reply exactly " + ack
			sum := sha256.Sum256([]byte(prompt))
			f.op.ResumeEvidence.Probe.PromptHash = hex.EncodeToString(sum[:])
			if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			var err error
			f.op, _, err = f.store.GetOrchestratorReplacement(ctx, f.op.ID)
			if err != nil {
				t.Fatal(err)
			}
			first, err := f.service.EnsureReplacementChatProbe(ctx, f.op, prompt, false)
			if err != nil || first.TurnID == "" || first.Owner != f.op.ResumeEvidence.AfterOwner || first.NativeID != f.op.ResumeEvidence.AfterNativeID {
				t.Fatal("probe not bound to resumed owner", first, err)
			}
			again, err := f.service.EnsureReplacementChatProbe(ctx, f.op, prompt, false)
			if err != nil || again.TurnID != first.TurnID || again.Uncertain || len(f.provider.sentTexts()) != 1 {
				t.Fatal("probe receipt replayed", again, err)
			}
			if _, err := f.service.EnsureReplacementChatProbe(ctx, f.op, prompt+" changed", true); err == nil || len(f.provider.sentTexts()) != 1 {
				t.Fatal("changed probe accepted")
			}
			answer := ack
			if reply == "wrong ack" {
				answer = "I am ready, but this is not the operation nonce"
			}
			f.provider.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-2"}, ports.ChatEvent{Kind: ports.ChatEventMessageCompleted, ProviderTurnID: "provider-turn-2", ProviderItemID: "probe-answer", Text: answer}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2", TurnState: domain.TurnStateCompleted})
			conversation, err := f.store.ConversationForSession(ctx, f.target.ID)
			if err != nil {
				t.Fatal(err)
			}
			awaitStoreSnapshot(t, f.store, conversation.ID, func(snapshot store.ConversationSnapshot) bool {
				for _, turn := range snapshot.Turns {
					if turn.ID == first.TurnID && turn.State == domain.TurnStateCompleted {
						return true
					}
				}
				return false
			})
			if reply != "unknown ack" {
				native := f.op.ResumeEvidence.AfterNativeID
				if reply == "child ack" {
					native = "child-native"
				}
				payload := fmt.Sprintf(`{"providerTurnId":%q,"providerConversationId":%q}`, "provider-turn-2", native)
				if _, err := f.store.ProjectProviderEvent(ctx, conversation.ID, f.target.ID, f.op.ResumeEvidence.AfterOwner.ControllerGeneration, "probe-identity", "turn.started", payload, time.Now(), func(context.Context) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			observed, err := f.service.ObserveReplacementChatSuccessor(ctx, f.op)
			if reply == "main ack" || reply == "wrong ack" {
				if err != nil || observed.Owner != f.op.ResumeEvidence.AfterOwner || observed.ControlTurn == nil || observed.ControlTurn.ID != first.TurnID || observed.ControlAcknowledged != (reply == "main ack") {
					t.Fatal("wrong probe completion facts", reply, observed, err)
				}
			} else if err == nil {
				t.Fatal("child/unknown probe accepted", reply, observed)
			}
			f.op.ResumeEvidence.Probe.Nonce = "55555555-5555-4555-8555-555555555555"
			if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
				t.Fatal("probe nonce was mutable", saved, err)
			}
		})
	}
}
