package chat_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type racingBackgroundConversation struct {
	*fakeConversation
	atStart, raced bool
	prepared       ports.ChatUserMessage
}

func (c *racingBackgroundConversation) SendTurn(_ context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	c.mu.Lock()
	c.prepared = msg
	race := !c.atStart && !c.raced
	if race {
		c.raced = true
	}
	c.mu.Unlock()
	if race {
		c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "background", ProviderInitiated: true})
		return ports.ChatTurnRef{}, ports.ErrChatTurnNotSentBusy
	}
	return ports.ChatTurnRef{ProviderTurnID: "ao-next"}, nil
}

func (c *racingBackgroundConversation) StartDeferredTurn(string) error {
	c.mu.Lock()
	race := c.atStart && !c.raced
	if race {
		c.raced = true
	} else {
		c.sent = append(c.sent, c.prepared)
	}
	c.mu.Unlock()
	if race {
		c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "background", ProviderInitiated: true})
		return ports.ErrChatTurnNotSentBusy
	}
	return nil
}

func (*racingBackgroundConversation) DiscardDeferredTurn(string) {}

func TestHarnessBackgroundDispatchRaceRequeuesUnsentInput(t *testing.T) {
	for _, atStart := range []bool{false, true} {
		name := "before-prepare"
		if atStart {
			name = "after-bind"
		}
		t.Run(name, func(t *testing.T) {
			conv := &racingBackgroundConversation{fakeConversation: newFakeConversation(), atStart: atStart}
			h := newHarnessWithConversation(t, conv)
			turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "preserve my input", ClientMessageID: name})
			if err != nil || turn.State != domain.TurnStateQueued {
				t.Fatalf("unsent input was not requeued: %+v %v", turn, err)
			}
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				queued, running := false, false
				for _, row := range s.Turns {
					queued = queued || (row.ID == turn.ID && row.State == domain.TurnStateQueued && row.ProviderTurnID == "")
					running = running || (row.ProviderTurnID == "background" && row.State == domain.TurnStateRunning)
				}
				return len(s.Turns) == 2 && queued && running
			})
			if len(conv.sentMessages()) != 0 {
				t.Fatal("input reached the provider during background work")
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				for _, row := range s.Turns {
					if row.ID == turn.ID && row.State == domain.TurnStateRunning && row.ProviderTurnID == "ao-next" {
						return true
					}
				}
				return false
			})
			if messages := conv.sentMessages(); len(messages) != 1 || messages[0].Text != "preserve my input" {
				t.Fatalf("input lost or duplicated: %+v", messages)
			}
		})
	}
}

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
