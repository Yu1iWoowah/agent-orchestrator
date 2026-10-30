package chat_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
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
	id := "ao-next"
	if !race {
		c.turnSeq++
		if c.turnSeq > 1 {
			id = fmt.Sprintf("ao-next-%d", c.turnSeq)
		}
	}
	c.mu.Unlock()
	if race {
		c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "background", ProviderInitiated: true})
		return ports.ChatTurnRef{}, ports.ErrChatTurnNotSentBusy
	}
	return ports.ChatTurnRef{ProviderTurnID: id}, nil
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
			h.advance(time.Second)
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				for _, row := range s.Turns {
					if row.State == domain.TurnStateFailed {
						t.Fatalf("retry failed: %+v", row)
					}
					wantProviderID := "ao-next"
					if atStart {
						wantProviderID = "ao-next-2"
					}
					if row.ID == turn.ID && row.State == domain.TurnStateRunning && row.ProviderTurnID == wantProviderID {
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

func TestHarnessBackgroundDispatchRaceRequeuesBatch(t *testing.T) {
	for _, atStart := range []bool{false, true} {
		t.Run(fmt.Sprint(atStart), func(t *testing.T) {
			conv := &racingBackgroundConversation{fakeConversation: newFakeConversation(), atStart: atStart}
			h := newHarnessWithConversation(t, conv)
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "prior", ProviderInitiated: true})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 })
			var ids []string
			for _, text := range []string{"lead", "follower"} {
				turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: text, ClientMessageID: text})
				if err != nil || turn.State != domain.TurnStateQueued {
					t.Fatalf("queue: %+v %v", turn, err)
				}
				ids = append(ids, turn.ID)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "prior", TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				queued, background := 0, false
				for _, r := range s.Turns {
					if r.State == domain.TurnStateQueued {
						queued++
					}
					background = background || r.ProviderTurnID == "background"
				}
				return queued == 2 && background
			})
			h.advance(time.Second)
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				for _, r := range s.Turns {
					if r.State == domain.TurnStateFailed {
						t.Fatalf("batch retry failed: %+v", r)
					}
				}
				follower, err := h.st.TurnByID(context.Background(), ids[1])
				return err == nil && follower.State == domain.TurnStateCompleted
			})
			lead, err := h.st.TurnByID(context.Background(), ids[0])
			if err != nil || lead.State != domain.TurnStateRunning {
				t.Fatalf("lead: %+v %v", lead, err)
			}
			messages := conv.sentMessages()
			if len(messages) != 1 {
				t.Fatalf("batch delivery count: %+v", messages)
			}
			items := parseDeliveredBatch(t, messages[0].Text)
			if len(items) != 2 || items[0].Text != "lead" || items[1].Text != "follower" {
				t.Fatalf("batch order/content: %+v", items)
			}
		})
	}
}

type cancelledBackgroundConversation struct {
	*racingBackgroundConversation
	cancel context.CancelFunc
}

func (c *cancelledBackgroundConversation) StartDeferredTurn(id string) error {
	err := c.racingBackgroundConversation.StartDeferredTurn(id)
	if errors.Is(err, ports.ErrChatTurnNotSentBusy) {
		c.cancel()
	}
	return err
}

func TestHarnessBackgroundCancelledRequestRequeuesUnsentInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conv := &cancelledBackgroundConversation{&racingBackgroundConversation{fakeConversation: newFakeConversation(), atStart: true}, cancel}
	h := newHarnessWithConversation(t, conv)
	turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "cancelled input", ClientMessageID: "cancelled"})
	if err != nil || turn.State != domain.TurnStateQueued {
		t.Fatalf("cancelled request stranded input: %+v %v", turn, err)
	}
	h.advance(time.Second)
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: domain.TurnStateCompleted})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(conv.sentMessages()) == 1 })
	next, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "next input", ClientMessageID: "next"})
	if err != nil || next.State != domain.TurnStateQueued {
		t.Fatalf("later intake: %+v %v", next, err)
	}
	running, err := h.st.TurnByID(context.Background(), turn.ID)
	if err != nil || running.State != domain.TurnStateRunning {
		t.Fatalf("cancelled input did not resume: %+v %v", running, err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: running.ProviderTurnID, TurnState: domain.TurnStateCompleted})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(conv.sentMessages()) == 2 })
	messages := conv.sentMessages()
	if messages[0].Text != "cancelled input" || messages[1].Text != "next input" {
		t.Fatalf("lost/duplicated input: %+v", messages)
	}
}

type failedRequeueStore struct{ chatsvc.Store }

func (s *failedRequeueStore) RequeueUnsentTurn(context.Context, string, string, string, []string) error {
	return errors.New("requeue-injected")
}
func TestHarnessBackgroundRequeueFailureReleasesController(t *testing.T) {
	for _, atStart := range []bool{false, true} {
		t.Run(fmt.Sprint(atStart), func(t *testing.T) {
			conv := &racingBackgroundConversation{fakeConversation: newFakeConversation(), atStart: atStart}
			h := newHarnessWithConversationAndStore(t, conv, func(st *store.Store) chatsvc.Store { return &failedRequeueStore{Store: st} })
			_, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "failed cleanup", ClientMessageID: "failed"})
			if err == nil {
				t.Fatal("store failure ignored")
			}
			s := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 2 })
			for _, r := range s.Turns {
				if r.ProviderTurnID != "background" && r.State != domain.TurnStateFailed {
					t.Errorf("unsent row not settled: %+v", r)
				}
			}
			for _, id := range h.ctrl.DispatchingTurnIDs() {
				if id != "" {
					t.Errorf("dispatch ownership retained: %q", id)
				}
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				for _, r := range s.Turns {
					if r.ProviderTurnID == "background" {
						return r.State == domain.TurnStateCompleted
					}
				}
				return false
			})
			next, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "fresh input", ClientMessageID: "fresh"})
			if err != nil || next.State != domain.TurnStateRunning || len(conv.sentMessages()) != 1 {
				t.Fatalf("controller stranded after cleanup error: next=%+v err=%v sent=%+v", next, err, conv.sentMessages())
			}
		})
	}
}

type completionBusyConversation struct {
	*fakeConversation
	refused chan struct{}
	busy    bool
}

func (c *completionBusyConversation) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	c.mu.Lock()
	busy := c.busy
	if busy {
		c.busy = false
		close(c.refused)
	}
	c.mu.Unlock()
	if busy {
		return ports.ChatTurnRef{}, ports.ErrChatTurnNotSentBusy
	}
	return c.fakeConversation.SendTurn(ctx, msg)
}
func (*completionBusyConversation) StartDeferredTurn(string) error { return nil }
func (*completionBusyConversation) DiscardDeferredTurn(string)     {}
func TestHarnessBackgroundReadyRetriesUnsentQueue(t *testing.T) {
	conv := &completionBusyConversation{fakeConversation: newFakeConversation(), busy: true, refused: make(chan struct{})}
	h := newHarnessWithConversation(t, conv)
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "background", ProviderInitiated: true})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 })
	turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "queued input"})
	if err != nil || turn.State != domain.TurnStateQueued {
		t.Fatalf("queue: %+v %v", turn, err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: domain.TurnStateCompleted})
	select {
	case <-conv.refused:
	case <-time.After(5 * time.Second):
		t.Fatal("completion did not attempt dispatch")
	}
	h.advance(time.Second)
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}, ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "background", ProviderItemID: "after-ready", ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: "after-ready"})
	s := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		for _, a := range s.Activities {
			if a.Summary == "after-ready" {
				return true
			}
		}
		return false
	})
	for _, r := range s.Turns {
		if r.ID == turn.ID && r.State != domain.TurnStateRunning {
			t.Fatalf("Ready did not retry input: %+v sent=%+v", r, conv.sentMessages())
		}
	}
	if messages := conv.sentMessages(); len(messages) != 1 || messages[0].Text != "queued input" {
		t.Fatalf("delivery: %+v", messages)
	}
}

func TestHarnessBackgroundFailureHoldsRequeuedInputAfterReady(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateFailed, domain.TurnStateInterrupted} {
		t.Run(string(state), func(t *testing.T) {
			conv := &racingBackgroundConversation{fakeConversation: newFakeConversation(), atStart: true}
			h := newHarnessWithConversation(t, conv)
			turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "held input"})
			if err != nil || turn.State != domain.TurnStateQueued {
				t.Fatalf("requeue: %+v %v", turn, err)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: state}, ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}, ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "background", ProviderItemID: "ready-fence", ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: "ready-fence"})
			s := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				for _, a := range s.Activities {
					if a.Summary == "ready-fence" {
						return true
					}
				}
				return false
			})
			for _, r := range s.Turns {
				if r.ID == turn.ID && r.State != domain.TurnStateQueued {
					t.Fatalf("non-success background released queue: %+v", r)
				}
			}
			if len(conv.sentMessages()) != 0 {
				t.Fatalf("held input sent: %+v", conv.sentMessages())
			}
		})
	}
}

func TestHarnessBackgroundBatchRequeueFailureSettlesFollowers(t *testing.T) {
	for _, atStart := range []bool{false, true} {
		t.Run(fmt.Sprint(atStart), func(t *testing.T) {
			conv := &racingBackgroundConversation{fakeConversation: newFakeConversation(), atStart: atStart}
			h := newHarnessWithConversationAndStore(t, conv, func(st *store.Store) chatsvc.Store { return &failedRequeueStore{Store: st} })
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "prior", ProviderInitiated: true})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 })
			for _, text := range []string{"lead", "follower"} {
				turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: text})
				if err != nil || turn.State != domain.TurnStateQueued {
					t.Fatalf("queue: %+v %v", turn, err)
				}
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "prior", TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				failed := 0
				for _, r := range s.Turns {
					if r.State == domain.TurnStateFailed {
						failed++
					}
				}
				return failed == 2
			})
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				for _, r := range s.Turns {
					if r.ProviderTurnID == "background" {
						return r.State == domain.TurnStateCompleted
					}
				}
				return false
			})
			next, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "fresh input"})
			if err != nil || next.State != domain.TurnStateRunning || len(conv.sentMessages()) != 1 {
				t.Fatalf("batch cleanup stranded controller: %+v %v sent=%+v", next, err, conv.sentMessages())
			}
		})
	}
}
