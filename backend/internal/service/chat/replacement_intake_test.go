package chat_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestReplacementTUIIntakeKeepsCanonicalIdentityAndStructuredPayloadWithoutController(t *testing.T) {
	for _, which := range []string{"current", "no operation", "worker", "rotated owner"} {
		t.Run(which, func(t *testing.T) {
			ctx := context.Background()
			st, workerID := openProvisioningStore(t, domain.SessionProvisionReady)
			now := time.Now().UTC()
			metadata := domain.SessionMetadata{WorkspacePath: t.TempDir(), RuntimeHandleID: "source-handle", RuntimeLaunchID: "source-launch", AgentSessionID: "source-native", AgentSessionIDLaunchID: "source-launch", BrowserCapabilityVerifier: "source-verifier",
				LatestUserPrompt: "verified native prompt", LatestUserPromptAt: now, ConversationCheckpointState: domain.ConversationCheckpointComplete, ConversationCheckpointGeneration: "source-launch", ConversationCheckpointNativeID: "source-native", ConversationCheckpointTurnID: "verified-native-turn"}
			rec, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeTUI, Activity: domain.Activity{State: domain.ActivityIdle}, CreatedAt: now, UpdatedAt: now, Metadata: metadata})
			if err != nil {
				t.Fatal(err)
			}
			op := domain.OrchestratorReplacement{SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64), ProjectID: rec.ProjectID, SourceSessionID: rec.ID, SourceOwner: rec.ControllerOwner(), SourceCapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)}
			if which != "no operation" {
				if _, _, err := st.CreateOrchestratorReplacement(ctx, op); err != nil {
					t.Fatal(err)
				}
			}
			id := rec.ID
			if which == "worker" {
				id = workerID
			}
			if which == "rotated owner" {
				rec.Metadata.BrowserCapabilityVerifier = "rotated"
				if err := st.UpdateSession(ctx, rec); err != nil {
					t.Fatal(err)
				}
			}
			svc := provisioningService(t, st)
			msg := ports.ChatUserMessage{Text: "keep this input", ClientMessageID: "original-client", Origin: domain.MessageOriginHuman, Content: []ports.ChatContent{{Type: "image", Data: "aW1hZ2U=", MIMEType: "image/png"}}}
			turn, err := svc.QueueReplacementIntake(ctx, id, msg)
			if which != "current" {
				if err == nil {
					t.Fatalf("%s accepted intake without ownership: %+v", which, turn)
				}
				return
			}
			if err != nil || turn.ID == "" || turn.State != domain.TurnStateQueued {
				t.Fatalf("intake not retained: %+v %v", turn, err)
			}
			replayed, err := svc.QueueReplacementIntake(ctx, id, msg)
			if err != nil || replayed.ID != turn.ID {
				t.Fatalf("replay changed receipt: %+v %v", replayed, err)
			}
			changed := msg
			changed.Text = "different payload"
			if _, err := svc.QueueReplacementIntake(ctx, id, changed); !errors.Is(err, domain.ErrClientMessageConflict) {
				t.Fatalf("client id overwrite accepted: %v", err)
			}
			conv, err := st.ConversationForSession(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := st.LoadConversationSnapshot(ctx, conv.ID)
			if err != nil || len(rows.Messages) != 1 || rows.Messages[0].ClientMessageID != msg.ClientMessageID || !strings.Contains(rows.Messages[0].DeliveryContentJSON, "image/png") || len(rows.Turns) != 1 || rows.Turns[0].ProviderTurnID != "" {
				t.Fatalf("canonical payload/identity changed: %+v %v", rows, err)
			}
			if _, err := st.NextQueuedTurn(ctx, conv.ID); !errors.Is(err, domain.ErrNoQueuedTurn) {
				t.Fatalf("held TUI intake dispatchable: %v", err)
			}
			if svc.HasLiveChatController(id) {
				t.Fatal("intake started a Chat controller for TUI source")
			}
			current, _, err := st.GetSession(ctx, id)
			if err != nil || current.Metadata.ConversationCheckpointState != metadata.ConversationCheckpointState || current.Metadata.ConversationCheckpointTurnID != metadata.ConversationCheckpointTurnID || current.Metadata.LatestUserPrompt != metadata.LatestUserPrompt || !current.Metadata.LatestUserPromptAt.Equal(metadata.LatestUserPromptAt) {
				t.Fatalf("queued TUI input rewrote native checkpoint: %+v err=%v", current.Metadata, err)
			}
		})
	}
}

func TestReplacementExistingActiveTurnSettlesWithoutDispatchingHeldFollowers(t *testing.T) {
	h, _, _, _ := replacementControlHarnessWithActiveTurn(t, true)
	ctx := context.Background()
	held, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "after acceptance", ClientMessageID: "held-follower", Origin: domain.MessageOriginHuman})
	if err != nil || held.State != domain.TurnStateQueued {
		t.Fatal(held, err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1", TurnState: domain.TurnStateCompleted})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 2 && s.Turns[0].State == domain.TurnStateCompleted && s.Turns[1].ID == held.ID && s.Turns[1].State == domain.TurnStateQueued
	})
	if err := h.svc.DrainQueued(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	if len(h.conv.sentTexts()) != 1 {
		t.Fatal("held follower reached provider", h.conv.sentTexts())
	}
	row, err := h.st.TurnByID(ctx, held.ID)
	if err != nil || row.State != domain.TurnStateQueued || row.ProviderTurnID != "" || row.StartedAt != nil {
		t.Fatal("held receipt changed", row, err)
	}
}
