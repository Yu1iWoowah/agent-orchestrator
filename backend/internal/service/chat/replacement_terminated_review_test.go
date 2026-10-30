package chat_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestTerminatedOrchestratorWithoutReplacementKeepsControllerNotReady(t *testing.T) {
	for _, path := range []string{"send", "relay"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			st, _ := openProvisioningStore(t, domain.SessionProvisionReady)
			now := time.Now().UTC()
			source, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, IsTerminated: true, Activity: domain.Activity{State: domain.ActivityExited}, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			svc := provisioningService(t, st)
			if path == "send" {
				_, err = svc.Send(ctx, source.ID, ports.ChatUserMessage{Text: "no running controller", ClientMessageID: "rejected", Origin: domain.MessageOriginHuman})
			} else {
				_, err = svc.RelaySessionChatTurn(ctx, source.ID, "no running controller", "rejected", ports.MessageDeliveryOptions{})
			}
			if !errors.Is(err, chatsvc.ErrNoController) {
				t.Fatalf("expected legacy controller-not-ready refusal: %v", err)
			}
			conversation, err := st.ConversationForSession(ctx, source.ID)
			if errors.Is(err, store.ErrConversationNotFound) {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			rows, err := st.LoadConversationSnapshot(ctx, conversation.ID)
			if err != nil || len(rows.Messages) != 0 || len(rows.Turns) != 0 {
				t.Fatalf("refused input acquired canonical rows: messages=%d turns=%d err=%v", len(rows.Messages), len(rows.Turns), err)
			}
		})
	}
}
