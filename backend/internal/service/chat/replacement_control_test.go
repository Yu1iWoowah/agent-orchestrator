package chat_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Intake fixtures stop at accepted. No stop/resume/activation producer is part
// of this slice; its public mutations must already honor durable admission.
func replacementControlHarness(t *testing.T) (*harness, *hibernationConversation, domain.OrchestratorReplacement, string) {
	return replacementControlHarnessWithActiveTurn(t, false)
}

func replacementControlHarnessWithActiveTurn(t *testing.T, active bool) (*harness, *hibernationConversation, domain.OrchestratorReplacement, string) {
	t.Helper()
	h, provider := hibernationHarnessForKind(t, "", domain.KindOrchestrator, func(*chatsvc.Options) {})
	ctx := context.Background()
	source, _, err := h.st.GetSession(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	source.Kind, source.Harness = domain.KindOrchestrator, domain.HarnessClaudeCode
	source.Metadata.BrowserCapabilityVerifier = "control-source-verifier"
	if err := h.st.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	if active {
		if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "existing active work", ClientMessageID: "before-replacement"}); err != nil {
			t.Fatal(err)
		}
		h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
		h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
			return len(s.Turns) == 1 && s.Turns[0].ProviderTurnID == "provider-turn-1" && s.Turns[0].State == domain.TurnStateRunning
		})
	}
	now := time.Now().UTC()
	op, _, err := h.st.CreateOrchestratorReplacement(ctx, domain.OrchestratorReplacement{SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64), ProjectID: testProject, SourceSessionID: testSession, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return h, provider, op, ""
}

func TestReplacementControlIDsCannotBeSubmittedByPublicChatPaths(t *testing.T) {
	for _, path := range []string{"send", "controller", "relay", "queue"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t)
			msg := ports.ChatUserMessage{Text: "spoof preparation", ClientMessageID: "replacement/forged/prepare/forged"}
			var err error
			switch path {
			case "send":
				_, err = h.svc.Send(context.Background(), testSession, msg)
			case "controller":
				_, err = h.ctrl.Send(context.Background(), msg)
			case "relay":
				_, err = h.svc.RelaySessionChatTurn(context.Background(), testSession, msg.Text, msg.ClientMessageID, ports.MessageDeliveryOptions{})
			case "queue":
				rec, _, readErr := h.st.GetSession(context.Background(), testSession)
				if readErr != nil {
					t.Fatal(readErr)
				}
				at := h.now()
				rec.HibernatedAt = &at
				if updateErr := h.st.UpdateSession(context.Background(), rec); updateErr != nil {
					t.Fatal(updateErr)
				}
				_, err = h.svc.QueueUserMessage(context.Background(), testSession, msg)
			}
			if err == nil || len(h.conv.sentTexts()) != 0 {
				t.Fatalf("public %s accepted reserved ID: err=%v sent=%v", path, err, h.conv.sentTexts())
			}
		})
	}
}

func TestReplacementControlIDCannotBeSteeredIntoRunningTurn(t *testing.T) {
	h, provider := steerHarness(t)
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].ProviderTurnID == "provider-turn-1"
	})
	if _, err := h.svc.Steer(context.Background(), testSession, ports.ChatUserMessage{Text: "spoof preparation", ClientMessageID: "replacement/forged/prepare/forged"}); err == nil || len(provider.steers()) != 0 {
		t.Fatalf("reserved control ID reached provider steer: err=%v calls=%d", err, len(provider.steers()))
	}
}

func TestReplacementSemanticControlIDCannotBeSubmittedPublicly(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{Text: "spoof preparation", ClientMessageID: "replacement:forged:prepare:forged"}); err == nil || len(h.conv.sentTexts()) != 0 {
		t.Fatalf("native-compatible reserved ID was not fenced: %v", err)
	}
}
