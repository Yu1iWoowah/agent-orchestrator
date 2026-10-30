package chat_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestSessionHistoryIncludesHarnessTriggeredReply(t *testing.T) {
	conv := newFakeConversation()
	h := newHarnessWithConversation(t, conv)
	conv.emit(
		ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "background-cycle", ProviderInitiated: true},
		ports.ChatEvent{Kind: ports.ChatEventMessageCompleted, ProviderTurnID: "background-cycle", ProviderItemID: "background-answer", Text: "Background finished"},
		ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background-cycle", TurnState: domain.TurnStateCompleted},
	)
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateCompleted
	})
	history, total, err := h.svc.SessionHistory(context.Background(), testSession, 0, 10)
	if err != nil || total != 1 || len(history) != 1 || history[0].AgentResponse != "Background finished" {
		t.Fatalf("persisted harness reply missing from session history: history=%+v total=%d err=%v", history, total, err)
	}
}

func TestAOPromptQueuesBehindHarnessTriggeredTurn(t *testing.T) {
	conv := newFakeConversation()
	h := newHarnessWithConversation(t, conv)
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "background", ProviderInitiated: true})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateRunning
	})
	queued, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "next user prompt"})
	if err != nil || queued.State != domain.TurnStateQueued || len(conv.sentMessages()) != 0 {
		t.Fatalf("AO prompt bypassed the harness turn: %+v, %v", queued, err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: domain.TurnStateCompleted})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 2 && s.Turns[1].State == domain.TurnStateRunning
	})
	if messages := conv.sentMessages(); len(messages) != 1 || messages[0].Text != "next user prompt" {
		t.Fatalf("queued AO prompt was lost or duplicated: %+v", messages)
	}
}
