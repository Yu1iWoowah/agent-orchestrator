package chat_test

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
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func replacementRuntimeControlHarness(t *testing.T) (*harness, *hibernationConversation, domain.OrchestratorReplacement, string) {
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
	now := time.Now().UTC()
	op, _, err := h.st.CreateOrchestratorReplacement(ctx, domain.OrchestratorReplacement{
		SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64),
		ProjectID: testProject, SourceSessionID: testSession, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier,
		Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	prompt := "write the operation-bound handoff with nonce exact-nonce"
	hash := sha256.Sum256([]byte(prompt))
	op.Phase = domain.ReplacementPreparingHandoff
	op.Preparation = &domain.ReplacementPreparation{LeaseEpoch: 7, AttemptID: "33333333-3333-4333-8333-333333333333", Nonce: "exact-nonce", ControlClientID: "replacement:" + op.ID + ":prepare:33333333-3333-4333-8333-333333333333", PromptHash: hex.EncodeToString(hash[:]), PromptIntentAt: &now, StartedAt: now, DeadlineAt: op.DeadlineAt}
	project, _, err := h.st.GetProject(ctx, string(testProject))
	if err != nil {
		t.Fatal(err)
	}
	op = sqlitetest.SeedReplacement(t, project.Path, op)
	op, _, err = h.st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return h, provider, op, prompt
}

func TestReplacementRuntimeChatPreparationRecoversReceiptWithoutResending(t *testing.T) {
	h, _, op, prompt := replacementRuntimeControlHarness(t)
	ctx := context.Background()
	missing, err := h.svc.EnsureReplacementChatPreparation(ctx, op, prompt, false)
	if err != nil || !missing.Uncertain || len(h.conv.sentTexts()) != 0 {
		t.Fatalf("lookup-only manufactured delivery: %+v %v", missing, err)
	}
	first, err := h.svc.EnsureReplacementChatPreparation(ctx, op, prompt, true)
	if err != nil || first.TurnID == "" || first.Uncertain || first.Owner != op.SourceOwner {
		t.Fatalf("bound delivery=%+v err=%v", first, err)
	}
	again, err := h.svc.EnsureReplacementChatPreparation(ctx, op, prompt, false)
	if err != nil || again.TurnID != first.TurnID || again.Uncertain || len(h.conv.sentTexts()) != 1 {
		t.Fatalf("retry did not recover exact receipt: first=%+v again=%+v sent=%v err=%v", first, again, h.conv.sentTexts(), err)
	}
	if _, err := h.svc.EnsureReplacementChatPreparation(ctx, op, prompt+" changed", true); err == nil || len(h.conv.sentTexts()) != 1 {
		t.Fatal("reused control ID accepted changed prompt")
	}
}

func TestReplacementRuntimeChatPreparationRefusesChangedCapabilityAndStoredPrompt(t *testing.T) {
	for _, which := range []string{"capability", "stored prompt"} {
		t.Run(which, func(t *testing.T) {
			h, _, op, prompt := replacementRuntimeControlHarness(t)
			ctx := context.Background()
			if which == "capability" {
				rec, _, err := h.st.GetSession(ctx, testSession)
				if err != nil {
					t.Fatal(err)
				}
				rec.Metadata.BrowserCapabilityVerifier = "rotated"
				if err := h.st.UpdateSession(ctx, rec); err != nil {
					t.Fatal(err)
				}
			} else {
				conv, err := h.st.ConversationForSession(ctx, testSession)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.st.AppendUserMessage(ctx, conv.ID, testSession, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "forged", ClientMessageID: op.Preparation.ControlClientID, Text: "different preparation", Origin: domain.MessageOriginAutomation}, "forged-turn", h.now()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.svc.EnsureReplacementChatPreparation(ctx, op, prompt, true); err == nil || len(h.conv.sentTexts()) != 0 {
				t.Fatalf("%s mismatch was accepted: %v", which, err)
			}
		})
	}
}

func TestReplacementRuntimePromptIntentCannotBeRewritten(t *testing.T) {
	h, _, op, _ := replacementRuntimeControlHarness(t)
	op.Preparation.PromptHash = strings.Repeat("b", 64)
	if saved, err := h.st.SaveOrchestratorReplacement(context.Background(), op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
		t.Fatalf("prompt identity was mutable after intent: saved=%t err=%v", saved, err)
	}
}

func TestReplacementRuntimeChatObservationRequiresMainControlTurnEvidence(t *testing.T) {
	for _, provenance := range []string{"main", "child", "unknown"} {
		t.Run(provenance, func(t *testing.T) {
			h, _, op, prompt := replacementRuntimeControlHarness(t)
			ctx := context.Background()
			receipt, err := h.svc.EnsureReplacementChatPreparation(ctx, op, prompt, true)
			if err != nil {
				t.Fatal(err)
			}
			h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1", TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateCompleted
			})
			if provenance != "unknown" {
				native := op.SourceOwner.ProviderConversationID
				if provenance == "child" {
					native = "native-child"
				}
				conv, err := h.st.ConversationForSession(ctx, testSession)
				if err != nil {
					t.Fatal(err)
				}
				archiveNativeTurnIdentity(t, h.st, conv.ID, op.SourceOwner.ControllerGeneration, "provider-turn-1", native, h.now())
			}
			observation, err := h.svc.ObserveReplacementChatSource(ctx, op)
			if provenance == "main" {
				if err != nil || observation.Provenance != domain.NativeThreadMain || observation.ControlTurn == nil || observation.ControlTurn.ID != receipt.TurnID || observation.ControlTurn.State != domain.TurnStateCompleted {
					t.Fatalf("main receipt not verified: %+v %v", observation, err)
				}
			} else if err == nil && observation.ControlTurn != nil && observation.ControlTurn.State == domain.TurnStateCompleted {
				t.Fatalf("%s receipt passed as completed main turn: %+v", provenance, observation)
			}
		})
	}
}

func TestReplacementRuntimeControlIDsCannotBeSubmittedByPublicChatPaths(t *testing.T) {
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

func TestReplacementRuntimeControlIDCannotBeSteeredIntoRunningTurn(t *testing.T) {
	h, provider := steerHarness(t)
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].ProviderTurnID == "provider-turn-1"
	})
	if _, err := h.svc.Steer(context.Background(), testSession, ports.ChatUserMessage{Text: "spoof preparation", ClientMessageID: "replacement/forged/prepare/forged"}); err == nil || len(provider.steers()) != 0 {
		t.Fatalf("reserved control ID reached provider steer: err=%v calls=%d", err, len(provider.steers()))
	}
}

func TestReplacementRuntimeSemanticControlIDCannotBeSubmittedPublicly(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{Text: "spoof preparation", ClientMessageID: "replacement:forged:prepare:forged"}); err == nil || len(h.conv.sentTexts()) != 0 {
		t.Fatalf("native-compatible reserved ID was not fenced: %v", err)
	}
}

func TestReplacementRuntimeChatStopReceiptRequiresActualOwnedShutdown(t *testing.T) {
	for _, which := range []string{"idle", "background", "detached", "marker only"} {
		t.Run(which, func(t *testing.T) {
			h, provider, op, prompt := replacementRuntimeControlHarness(t)
			ctx := context.Background()
			receipt, err := h.svc.EnsureReplacementChatPreparation(ctx, op, prompt, true)
			if err != nil {
				t.Fatal(err)
			}
			h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1", TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateCompleted
			})
			if err := h.svc.DrainQueued(ctx, testSession); err != nil {
				t.Fatal(err)
			}
			now := h.now()
			op.Phase, op.FrozenAt, op.SourceStopIntentAt = domain.ReplacementExportingTranscript, &now, &now
			op.Preparation.ControlTurnID, op.Preparation.VerifiedAt = receipt.TurnID, &now
			op.Transcript = &domain.ReplacementTranscript{AttemptID: "44444444-4444-4444-8444-444444444444", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}}
			project, _, err := h.st.GetProject(ctx, string(testProject))
			if err != nil {
				t.Fatal(err)
			}
			op = sqlitetest.ReplacementPriorFacts(op)
			op = sqlitetest.SeedReplacement(t, project.Path, op)
			op, _, err = h.st.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch which {
			case "background":
				provider.backgroundRunning = true
			case "detached":
				if err := h.svc.Stop(ctx, testSession); err != nil {
					t.Fatal(err)
				}
			case "marker only":
				if err := h.svc.Stop(ctx, testSession); err != nil {
					t.Fatal(err)
				}
				rec, _, err := h.st.GetSession(ctx, testSession)
				if err != nil {
					t.Fatal(err)
				}
				rec.HibernatedAt = &now
				if err := h.st.UpdateSession(ctx, rec); err != nil {
					t.Fatal(err)
				}
			}
			stop, err := h.svc.QuiesceReplacementChatSource(ctx, op)
			if which == "idle" {
				rec, _, readErr := h.st.GetSession(ctx, testSession)
				if err != nil || readErr != nil || !stop.MatchesSource(rec) || rec.IsTerminated || h.svc.HasLiveChatController(testSession) || provider.calls.Load() != 1 {
					t.Fatalf("owned shutdown did not yield proof: %+v err=%v read=%v calls=%d", stop, err, readErr, provider.calls.Load())
				}
				if _, err := h.svc.QuiesceReplacementChatSource(ctx, op); err == nil || provider.calls.Load() != 1 {
					t.Fatal("missing receipt retry fabricated another shutdown proof")
				}
			} else if err == nil || !stop.ConfirmedAt.IsZero() || provider.calls.Load() != 0 {
				t.Fatalf("%s fabricated stop proof: %+v err=%v calls=%d", which, stop, err, provider.calls.Load())
			}
		})
	}
}
