package chat_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type readyRetryFaultStore struct {
	chatsvc.Store
	atClaim        bool
	armed, faulted atomic.Bool
	requeued       chan struct{}
	once           sync.Once
}

func (s *readyRetryFaultStore) RequeueUnsentTurn(ctx context.Context, cid, id, pid string, followers []string) error {
	err := s.Store.RequeueUnsentTurn(ctx, cid, id, pid, followers)
	if err == nil {
		s.once.Do(func() { close(s.requeued) })
	}
	return err
}
func (s *readyRetryFaultStore) ListQueuedBatch(ctx context.Context, cid string, limit int) ([]domain.QueuedTurn, error) {
	if !s.atClaim && s.armed.CompareAndSwap(true, false) {
		s.faulted.Store(true)
		return nil, errors.New("ready-list-injected")
	}
	return s.Store.ListQueuedBatch(ctx, cid, limit)
}
func (s *readyRetryFaultStore) ClaimQueuedBatch(ctx context.Context, cid string, dispatch bool, limit int, at time.Time) ([]domain.QueuedTurn, error) {
	if s.atClaim && s.armed.CompareAndSwap(true, false) {
		s.faulted.Store(true)
		return nil, errors.New("ready-claim-injected")
	}
	return s.Store.ClaimQueuedBatch(ctx, cid, dispatch, limit, at)
}
func TestHarnessBackgroundReadyRetriesAfterTransientStoreFailure(t *testing.T) {
	for _, atClaim := range []bool{false, true} {
		name := "list"
		if atClaim {
			name = "claim"
		}
		t.Run(name, func(t *testing.T) {
			conv := &completionBusyConversation{fakeConversation: newFakeConversation(), busy: true, refused: make(chan struct{})}
			st := &readyRetryFaultStore{atClaim: atClaim, requeued: make(chan struct{})}
			h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store { st.Store = db; return st })
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "background", ProviderInitiated: true})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 })
			turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "preserved retry", ClientMessageID: "retry"})
			if err != nil || turn.State != domain.TurnStateQueued {
				t.Fatalf("queue: %+v %v", turn, err)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: domain.TurnStateCompleted})
			select {
			case <-st.requeued:
			case <-time.After(5 * time.Second):
				t.Fatal("no busy requeue")
			}
			h.advance(time.Second)
			st.armed.Store(true)
			for attempt := 1; attempt <= 2; attempt++ {
				fence := fmt.Sprintf("ready-attempt-%d", attempt)
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}, ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "background", ProviderItemID: fence, ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: fence})
				h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
					for _, a := range s.Activities {
						if a.Summary == fence {
							return true
						}
					}
					return false
				})
				saved, err := h.st.TurnByID(context.Background(), turn.ID)
				if err != nil || !st.faulted.Load() {
					t.Fatalf("store failure not exercised: %+v %v", saved, err)
				}
				if attempt == 1 {
					if saved.State != domain.TurnStateQueued || len(conv.sentMessages()) != 0 {
						t.Fatalf("failed retry lost input: %+v sent=%+v", saved, conv.sentMessages())
					}
				} else if saved.State != domain.TurnStateRunning || len(conv.sentMessages()) != 1 {
					t.Fatalf("second Ready did not retry after transient %s error: state=%s sent=%+v", name, saved.State, conv.sentMessages())
				}
			}
			if messages := conv.sentMessages(); messages[0].Text != "preserved retry" {
				t.Fatalf("input changed: %+v", messages)
			}
		})
	}
}

type terminalBeforeRequeueStore struct {
	chatsvc.Store
	state    domain.TurnState
	at       time.Time
	requeued chan struct{}
}

func (s *terminalBeforeRequeueStore) RequeueUnsentTurn(ctx context.Context, cid, id, pid string, followers []string) error {
	for _, turnID := range append([]string{id}, followers...) {
		if err := s.SettleTurnByID(ctx, turnID, s.state, "original terminal outcome", s.at); err != nil {
			return err
		}
	}
	err := s.Store.RequeueUnsentTurn(ctx, cid, id, pid, followers)
	close(s.requeued)
	return err
}
func TestHarnessBackgroundRequeueFailurePreservesTerminalOutcome(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateCompleted, domain.TurnStateFailed, domain.TurnStateInterrupted} {
		for _, atStart := range []bool{false, true} {
			for _, batch := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/after-bind=%t/batch=%t", state, atStart, batch), func(t *testing.T) {
					conv := &racingBackgroundConversation{fakeConversation: newFakeConversation(), atStart: atStart}
					st := &terminalBeforeRequeueStore{state: state, at: time.Date(2026, 8, 2, 9, 59, 0, 0, time.UTC), requeued: make(chan struct{})}
					h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store { st.Store = db; return st })
					if batch {
						conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "prior", ProviderInitiated: true})
						h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 })
					}
					texts := []string{"lead"}
					if batch {
						texts = append(texts, "follower")
					}
					for _, text := range texts {
						turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: text, ClientMessageID: text})
						if batch && (err != nil || turn.State != domain.TurnStateQueued) {
							t.Fatalf("queue: %+v %v", turn, err)
						}
						if !batch && err == nil {
							t.Fatal("requeue unexpectedly accepted a terminal turn")
						}
					}
					if batch {
						conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "prior", TurnState: domain.TurnStateCompleted})
					}
					select {
					case <-st.requeued:
					case <-time.After(5 * time.Second):
						t.Fatal("terminal requeue was not exercised")
					}
					conv.emit(ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "background", ProviderItemID: "terminal-fence", ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: "terminal-fence"})
					snap := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
						for _, a := range s.Activities {
							if a.Summary == "terminal-fence" {
								return true
							}
						}
						return false
					})
					checked := 0
					for _, msg := range snap.Messages {
						if msg.ClientMessageID != "lead" && msg.ClientMessageID != "follower" {
							continue
						}
						saved, err := h.st.TurnByID(context.Background(), msg.TurnID)
						if err != nil || saved.State != state || saved.ErrorMessage != "original terminal outcome" || saved.CompletedAt == nil || !saved.CompletedAt.Equal(st.at) {
							t.Fatalf("terminal outcome overwritten: %+v err=%v wantState=%s", saved, err, state)
						}
						checked++
					}
					if checked != len(texts) || len(conv.sentMessages()) != 0 {
						t.Fatalf("terminal input lost or sent: checked=%d sent=%+v", checked, conv.sentMessages())
					}
				})
			}
		}
	}
}
