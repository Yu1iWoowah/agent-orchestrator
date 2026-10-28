package chat_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestRequiredHistoryAcceptsCompletedBatchWithoutOutput(t *testing.T) {
	for _, replay := range []string{"complete batch", "leader text only", "missing batch"} {
		t.Run(replay, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			active, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
				Text: "active", ClientMessageID: "active", Origin: domain.MessageOriginHuman,
			})
			if err != nil {
				t.Fatal(err)
			}
			h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted,
				ProviderTurnID: active.ProviderTurnID, ProviderConversationID: "thread-1"})
			h.advance(time.Second)
			leader, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
				Text: "first queued message", ClientMessageID: "first", Origin: domain.MessageOriginAutomation,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
				Text: "second queued message", ClientMessageID: "second", Origin: domain.MessageOriginAutomation,
			}); err != nil {
				t.Fatal(err)
			}
			h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted,
				ProviderTurnID: active.ProviderTurnID, ProviderConversationID: "thread-1", TurnState: domain.TurnStateCompleted})
			snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				turn, found := turnByID(s, leader.ID)
				return found && turn.ProviderTurnID != "" && len(h.conv.sentMessages()) == 2
			})
			batch, _ := turnByID(snapshot, leader.ID)
			input := h.conv.sentMessages()[1].Text
			if items := parseDeliveredBatch(t, input); len(items) != 2 || batch.ProviderInputText != input {
				t.Fatalf("provider input is not the recorded queued batch: %+v", batch)
			}
			h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted,
				ProviderTurnID: batch.ProviderTurnID, ProviderConversationID: "thread-1"})
			h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted,
				ProviderTurnID: batch.ProviderTurnID, ProviderConversationID: "thread-1", TurnState: domain.TurnStateCompleted})
			snapshot = h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				turn, found := turnByID(s, batch.ID)
				return found && turn.State == domain.TurnStateCompleted
			})
			for _, message := range snapshot.Messages {
				if message.Role == domain.MessageRoleAssistant || message.ProviderItemID != "" {
					t.Fatalf("fixture has output or item-ID evidence: %+v", message)
				}
			}
			for _, activity := range snapshot.Activities {
				if activity.ProviderItemID != "" {
					t.Fatalf("fixture has provider activity evidence: %+v", activity)
				}
			}
			if err := h.svc.Stop(ctx, testSession); err != nil {
				t.Fatal(err)
			}
			events := []ports.ChatEvent{
				{Kind: ports.ChatEventUserMessageCompleted, ProviderEventID: "active-user", ProviderTurnID: active.ProviderTurnID, ProviderItemID: "native-active", Text: "active"},
				{Kind: ports.ChatEventTurnCompleted, ProviderEventID: "active-complete", ProviderTurnID: active.ProviderTurnID, ProviderConversationID: "thread-1", TurnState: domain.TurnStateCompleted},
			}
			if replay != "missing batch" {
				text := input
				if replay == "leader text only" {
					text = "first queued message"
				}
				events = append(events,
					ports.ChatEvent{Kind: ports.ChatEventUserMessageCompleted, ProviderEventID: "batch-user", ProviderTurnID: batch.ProviderTurnID, ProviderItemID: "native-batch", Text: text},
					ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderEventID: "batch-complete", ProviderTurnID: batch.ProviderTurnID, ProviderConversationID: "thread-1", TurnState: domain.TurnStateCompleted})
			}
			provider := &nativeHistoryConversation{fakeConversation: newFakeConversation(), events: events}
			svc := chatsvc.New(chatsvc.Options{Store: h.st, Sessions: h.st, Reader: fullSnapshotReader(h.st),
				Drivers: fakeRegistry{driver: fakeDriver{conv: provider}}, NewID: uuid.NewString})
			t.Cleanup(func() { svc.StopAll(ctx) })
			resumeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_, err = svc.Start(resumeCtx, chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject,
				Harness: domain.HarnessCodex, ProviderConversationID: "thread-1", HistoryMode: ports.ChatHistoryRequired,
				HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict})
			if replay == "complete batch" {
				if err != nil {
					t.Fatalf("complete no-output batch history rejected: %v", err)
				}
			} else if !errors.Is(err, ports.ErrChatHistoryUnsettled) {
				t.Fatalf("incomplete batch replay accepted: %v", err)
			}
		})
	}
}
