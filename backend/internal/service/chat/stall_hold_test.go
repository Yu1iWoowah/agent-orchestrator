package chat_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// stallHoldStore lets a test fail the pending-delivery read or run code right
// after it, while the caller still holds the list it read.
type stallHoldStore struct {
	*store.Store
	failPending atomic.Bool
	afterRead   atomic.Pointer[func()]
}

func (s *stallHoldStore) PendingChatDeliveries(ctx context.Context, id domain.SessionID) ([]domain.ChatDeliveryAttempt, error) {
	if s.failPending.Load() {
		return nil, fmt.Errorf("journal unavailable")
	}
	attempts, err := s.Store.PendingChatDeliveries(ctx, id)
	if hook := s.afterRead.Load(); hook != nil && err == nil && len(attempts) > 0 {
		(*hook)()
	}
	return attempts, err
}

// gatedQueryConversation holds the delivery monitor inside its host query
// while gate is set, so a test knows where the monitor is.
type gatedQueryConversation struct {
	*deferredDeliveryConversation
	gate    atomic.Pointer[chan struct{}]
	blocked chan struct{}
}

func (c *gatedQueryConversation) QueryDeliveryAttempt(ctx context.Context, key ports.ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error) {
	if gate := c.gate.Load(); gate != nil {
		c.blocked <- struct{}{}
		<-*gate
	}
	return c.deferredDeliveryConversation.QueryDeliveryAttempt(ctx, key)
}

// stallHoldHarness sends a prompt whose recorded delivery has no progress yet,
// the state a host watchdog stall finds.
func stallHoldHarness(t *testing.T) (*harness, *gatedQueryConversation, *stallHoldStore, *settingsTestClock, domain.ChatDeliveryAttempt) {
	t.Helper()
	clock := newSettingsTestClock()
	stalled := &stalledDeliveryConversation{fakeConversation: newFakeConversation(), queryCount: make(chan string, 256)}
	conv := &gatedQueryConversation{deferredDeliveryConversation: &deferredDeliveryConversation{stalled}, blocked: make(chan struct{}, 8)}
	var wrapped *stallHoldStore
	h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *store.Store) chatsvc.Store {
		wrapped = &stallHoldStore{Store: st}
		return wrapped
	}, domain.HarnessCodex, func(options *chatsvc.Options) { options.Now = clock.Now; options.After = clock.After })
	stalled.setDeliveryClock(clock.Now, true)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "stalled prompt", ClientMessageID: "stalled"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	clock.awaitTimer(t, 30*time.Second)
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"})
	return h, conv, wrapped, clock, a
}

// emitStall delivers the watchdog failure the way the ACP driver does: without
// a delivery receipt.
func emitStall(t *testing.T, h *harness, conv *gatedQueryConversation) {
	t.Helper()
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateFailed, Err: fmt.Errorf("ACP session/prompt: %w", ports.ErrChatPromptStalled)}
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["stalled prompt"] == domain.TurnStateFailed
	})
}

func settleHostReceipt(conv *gatedQueryConversation, id string) {
	conv.mu.Lock()
	defer conv.mu.Unlock()
	receipt := conv.deliveryReceipts[id]
	receipt.Disposition, receipt.Source, receipt.Complete, receipt.ExecutionSettled = domain.ChatDeliveryAccepted, "provider_response", true, true
	conv.deliveryReceipts[id] = receipt
}

func sendQueued(t *testing.T, h *harness, text string) {
	t.Helper()
	if _, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: text, ClientMessageID: text, Origin: domain.MessageOriginHuman}); err != nil {
		t.Fatal(err)
	}
}

// Stop publishes its queue cutoff before the provider answers. When the answer
// is a watchdog stall held behind its unsettled delivery, the held drain must
// keep that cutoff, so settlement cancels the pre-Stop queue instead of sending it.
func TestStalledPromptHoldKeepsStopCutoff(t *testing.T) {
	h, conv, _, clock, a := stallHoldHarness(t)
	sendQueued(t, h, "before stop")
	if err := h.svc.Interrupt(t.Context(), testSession); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	sendQueued(t, h, "after stop")
	emitStall(t, h, conv)
	assertTurnStaysQueued(t, h, "before stop")

	settleHostReceipt(conv, a.ID)
	h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(30 * time.Second) })
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		states := turnStateByText(t, s)
		return states["before stop"] == domain.TurnStateInterrupted && states["after stop"] != domain.TurnStateQueued
	})
	for _, text := range conv.sentTexts() {
		if text == "before stop" {
			t.Fatalf("provider received input queued before Stop: %v", conv.sentTexts())
		}
	}
}

// A journal that cannot be read at the stall must still hold the queue: a later
// Send finds the controller busy, and the monitor's next successful read
// replaces the placeholder with the attempt whose settlement releases it.
func TestStalledPromptHoldSurvivesJournalReadFailure(t *testing.T) {
	h, conv, st, clock, a := stallHoldHarness(t)
	sendQueued(t, h, "queued")
	st.failPending.Store(true)
	emitStall(t, h, conv)
	sendQueued(t, h, "sent later")
	assertTurnStaysQueued(t, h, "queued")
	if sent := conv.sentTexts(); len(sent) != 1 {
		t.Fatalf("provider received %v while the journal could not say the stalled prompt settled", sent)
	}

	st.failPending.Store(false)
	settleHostReceipt(conv, a.ID)
	h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(30 * time.Second) })
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["queued"] != domain.TurnStateQueued
	})
}

// The monitor can settle the stalled prompt's attempt while the hold is being
// installed from an older read. The settlement must still find the hold to
// release, or every later drain stays busy behind a settled attempt.
func TestStalledPromptHoldSeesSettlementDuringInstall(t *testing.T) {
	h, conv, st, clock, a := stallHoldHarness(t)
	sendQueued(t, h, "queued")
	settleHostReceipt(conv, a.ID)

	// Park the monitor inside its host query, so the only pending-delivery read
	// at the stall is the hold's own.
	gate := make(chan struct{})
	conv.gate.Store(&gate)
	h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(30 * time.Second) })
	select {
	case <-conv.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery monitor never queried the host")
	}
	var fired atomic.Bool
	hook := func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		// Let the monitor settle the attempt in the gap between this read and
		// the install, then hand back the list read before it.
		conv.gate.Store(nil)
		close(gate)
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			if saved, err := h.st.ChatDeliveryAttempt(context.Background(), testSession, a.ID); err == nil && saved.ExecutionSettled {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	st.afterRead.Store(&hook)
	emitStall(t, h, conv)
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["queued"] != domain.TurnStateQueued
	})
	if !fired.Load() {
		t.Fatal("the settlement was not placed between the hold's read and install")
	}
}
