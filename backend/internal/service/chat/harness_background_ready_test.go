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

// Pause after SQLite has committed the reservation, with no database lock held.
type readyRetryBarrierStore struct {
	chatsvc.Store
	armed                      atomic.Bool
	claimed, release, requeued chan struct{}
	requeueOnce, releaseOnce   sync.Once
}

func (s *readyRetryBarrierStore) unblock() { s.releaseOnce.Do(func() { close(s.release) }) }
func (s *readyRetryBarrierStore) RequeueUnsentTurn(ctx context.Context, cid, id, pid string, followers []string) error {
	err := s.Store.RequeueUnsentTurn(ctx, cid, id, pid, followers)
	if err == nil {
		s.requeueOnce.Do(func() { close(s.requeued) })
	}
	return err
}
func (s *readyRetryBarrierStore) ClaimQueuedBatch(ctx context.Context, cid string, dispatch bool, limit int, at time.Time) ([]domain.QueuedTurn, error) {
	batch, err := s.Store.ClaimQueuedBatch(ctx, cid, dispatch, limit, at)
	if err == nil && s.armed.CompareAndSwap(true, false) {
		close(s.claimed)
		<-s.release
	}
	return batch, err
}

// Like ACP, an accepted prompt excludes every other prompt until completion.
type singleFlightReadyConversation struct {
	*completionBusyConversation
	active bool
}

func (c *singleFlightReadyConversation) SendTurn(_ context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	c.mu.Lock()
	if c.active {
		c.mu.Unlock()
		return ports.ChatTurnRef{}, errors.New("ACP conversation already has a turn in flight")
	}
	if c.busy {
		c.busy = false
		close(c.refused)
		c.mu.Unlock()
		return ports.ChatTurnRef{}, ports.ErrChatTurnNotSentBusy
	}
	c.active = true
	c.turnSeq++
	id := fmt.Sprintf("provider-turn-%d", c.turnSeq)
	c.sent = append(c.sent, msg)
	c.mu.Unlock()
	c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: id})
	return ports.ChatTurnRef{ProviderTurnID: id}, nil
}
func (c *singleFlightReadyConversation) finish(id string, state domain.TurnState) {
	c.mu.Lock()
	c.active = false
	c.mu.Unlock()
	c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: id, TurnState: state})
}
func (c *singleFlightReadyConversation) Interrupt(_ context.Context, id string) error {
	c.finish(id, domain.TurnStateInterrupted)
	return nil
}

func newPausedReadyRetry(t *testing.T) (*harness, *singleFlightReadyConversation, *readyRetryBarrierStore, domain.ConversationTurn) {
	t.Helper()
	conv := &singleFlightReadyConversation{completionBusyConversation: &completionBusyConversation{fakeConversation: newFakeConversation(), busy: true, refused: make(chan struct{})}}
	st := &readyRetryBarrierStore{claimed: make(chan struct{}), release: make(chan struct{}), requeued: make(chan struct{})}
	h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store { st.Store = db; return st })
	t.Cleanup(st.unblock)
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "background", ProviderInitiated: true})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 })
	queued, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "older queued input", ClientMessageID: "older"})
	if err != nil || queued.State != domain.TurnStateQueued {
		t.Fatalf("queue: %+v %v", queued, err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background", TurnState: domain.TurnStateCompleted})
	select {
	case <-st.requeued:
	case <-time.After(5 * time.Second):
		t.Fatal("completion did not requeue the refused input")
	}
	h.advance(time.Second)
	st.armed.Store(true)
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}, ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "background", ProviderItemID: "ready-fence", ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: "ready-fence"})
	select {
	case <-st.claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("Ready did not claim the old input")
	}
	return h, conv, st, queued
}

func awaitReadyRetryFence(t *testing.T, h *harness) {
	t.Helper()
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		for _, a := range s.Activities {
			if a.Summary == "ready-fence" {
				return true
			}
		}
		return false
	})
}

type readyConcurrentResult struct {
	turn domain.ConversationTurn
	err  error
}

func TestHarnessBackgroundReadySerializesConcurrentSend(t *testing.T) {
	h, conv, st, old := newPausedReadyRetry(t)
	started, done := make(chan struct{}), make(chan readyConcurrentResult, 1)
	go func() {
		close(started)
		turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "newer concurrent input", ClientMessageID: "newer"})
		done <- readyConcurrentResult{turn, err}
	}()
	<-started
	var fresh readyConcurrentResult
	early := false
	// The barrier creates the race; this deadline only observes whether Send waits.
	select {
	case fresh = <-done:
		early = true
	case <-time.After(100 * time.Millisecond):
	}
	st.unblock()
	if !early {
		select {
		case fresh = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Send did not finish")
		}
	}
	awaitReadyRetryFence(t, h)
	running, err := h.st.TurnByID(context.Background(), old.ID)
	if err != nil || running.State != domain.TurnStateRunning || fresh.err != nil || fresh.turn.State != domain.TurnStateQueued {
		t.Fatalf("Ready retry lost accepted input: old=%+v newer=%+v err=%v sent=%+v", running, fresh, err, conv.sentMessages())
	}
	if early {
		t.Fatal("newer Send passed the paused Ready dispatch")
	}
	if messages := conv.sentMessages(); len(messages) != 1 || messages[0].Text != "older queued input" {
		t.Fatalf("delivery bypassed queue: %+v", messages)
	}
	conv.finish(running.ProviderTurnID, domain.TurnStateCompleted)
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		for _, r := range s.Turns {
			if r.ID == fresh.turn.ID {
				return r.State == domain.TurnStateRunning
			}
		}
		return false
	})
	if messages := conv.sentMessages(); len(messages) != 2 || messages[1].Text != "newer concurrent input" {
		t.Fatalf("new input lost or duplicated: %+v", messages)
	}
}

func TestHarnessBackgroundReadySerializesConcurrentStop(t *testing.T) {
	h, conv, st, old := newPausedReadyRetry(t)
	started, done := make(chan struct{}), make(chan error, 1)
	go func() { close(started); done <- h.ctrl.Interrupt(context.Background()) }()
	<-started
	early := false
	var stopErr error
	select {
	case stopErr = <-done:
		early = true
	case <-time.After(100 * time.Millisecond):
	}
	st.unblock()
	if !early {
		select {
		case stopErr = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop did not finish")
		}
	}
	awaitReadyRetryFence(t, h)
	if stopErr != nil {
		t.Fatalf("Stop could not settle the Ready dispatch: %v", stopErr)
	}
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		for _, r := range s.Turns {
			if r.ID == old.ID {
				return r.State == domain.TurnStateInterrupted
			}
		}
		return false
	})
	if early {
		t.Fatal("Stop returned before the paused Ready dispatch finished")
	}
	h.advance(time.Second)
	fresh, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "after Stop", ClientMessageID: "after-stop"})
	if err != nil || fresh.State != domain.TurnStateRunning {
		t.Fatalf("post-Stop input blocked: %+v %v", fresh, err)
	}
	if messages := conv.sentMessages(); len(messages) != 2 || messages[0].Text != "older queued input" || messages[1].Text != "after Stop" {
		t.Fatalf("Stop delivery order: %+v", messages)
	}
}

func TestHarnessBackgroundReadySerializesConcurrentHandoff(t *testing.T) {
	h, conv, st, old := newPausedReadyRetry(t)
	defer h.ctrl.AbortHandoff()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		done <- h.ctrl.ArmHandoff(context.Background(), domain.SessionInterfaceTransitionInterrupt)
	}()
	<-started
	early := false
	var handoffErr error
	select {
	case handoffErr = <-done:
		early = true
	case <-time.After(100 * time.Millisecond):
	}
	st.unblock()
	if !early {
		select {
		case handoffErr = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("handoff did not finish")
		}
	}
	awaitReadyRetryFence(t, h)
	if handoffErr != nil {
		t.Fatal(handoffErr)
	}
	if early {
		t.Fatal("handoff returned before the paused Ready dispatch finished")
	}
	running, err := h.st.TurnByID(context.Background(), old.ID)
	if err != nil || running.State != domain.TurnStateRunning {
		t.Fatalf("accepted input lost at handoff: %+v %v", running, err)
	}
	if _, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "after handoff"}); !errors.Is(err, chatsvc.ErrControllerHandoff) {
		t.Fatalf("handoff did not fence new input: %v", err)
	}
	if messages := conv.sentMessages(); len(messages) != 1 || messages[0].Text != "older queued input" {
		t.Fatalf("handoff delivery: %+v", messages)
	}
}
