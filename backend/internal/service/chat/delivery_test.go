package chat_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func (f *fakeConversation) setDeliveryClock(now func() time.Time, deferred bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveryNow = now
	f.deliveryPrepared = deferred
}
func (f *fakeConversation) fakeDeliveryReceipt(attempt, turn string, settled bool) *domain.ChatDeliveryReceipt {
	if attempt == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deliveryNow == nil {
		f.deliveryNow = time.Now
	}
	if f.deliveryReceipts == nil {
		f.deliveryReceipts = map[string]domain.ChatDeliveryReceipt{}
	}
	r := domain.ChatDeliveryReceipt{AttemptID: attempt, ProviderTurnID: turn, ProviderRequestID: "fake-rpc-" + attempt, ProviderEpoch: "fake-native-epoch", Disposition: domain.ChatDeliveryAccepted, Source: "provider_response", Complete: true, ObservedAt: f.deliveryNow(), ExecutionSettled: settled}
	if f.deliveryPrepared && !settled {
		r.Disposition = domain.ChatDeliveryUnknown
		r.Source = "incomplete"
		r.Complete = false
	}
	f.deliveryReceipts[attempt] = r
	return &r
}
func (f *fakeConversation) QueryDeliveryAttempt(_ context.Context, key ports.ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.deliveryReceipts[key.AttemptID]
	if !ok {
		return r, ports.ErrChatDeliveryUnverified
	}
	return r, nil
}
func (f *fakeConversation) FenceAndFlushOutbound(ctx context.Context, keys []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	result := make([]domain.ChatDeliveryReceipt, 0, len(keys))
	for _, key := range keys {
		r, err := f.QueryDeliveryAttempt(ctx, key)
		if err != nil {
			return nil, err
		}
		r.CustodyFlushed = true
		result = append(result, r)
	}
	return result, nil
}

type stalledDeliveryConversation struct {
	*fakeConversation
	queryStarted chan struct{}
	queryCount   chan string
	timeout      bool
}

func (c *stalledDeliveryConversation) QueryDeliveryAttempt(ctx context.Context, key ports.ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error) {
	c.queryCount <- key.AttemptID
	if c.queryStarted != nil {
		c.queryStarted <- struct{}{}
	}
	if c.timeout {
		<-ctx.Done()
		return domain.ChatDeliveryReceipt{}, ctx.Err()
	}
	return c.fakeConversation.QueryDeliveryAttempt(ctx, key)
}
func (c *stalledDeliveryConversation) FenceAndFlushOutbound(ctx context.Context, keys []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	if c.timeout {
		return nil, ports.ErrChatDeliveryUnverified
	}
	return c.fakeConversation.FenceAndFlushOutbound(ctx, keys)
}

func deliveryHarness(t *testing.T, conv ports.ChatConversation, clock *settingsTestClock) *harness {
	return newHarnessWithConversationAndStoreForHarness(t, conv, func(st *store.Store) chatsvc.Store { return st }, domain.HarnessCodex, func(options *chatsvc.Options) { options.Now = clock.Now; options.After = clock.After })
}

func TestDeliveryStallQueriesOriginalAttemptAndKeepsAccepted(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-progress", true: "query-timeout"}[timeout], func(t *testing.T) {
			clock := newSettingsTestClock()
			conv := &stalledDeliveryConversation{fakeConversation: newFakeConversation(), queryStarted: make(chan struct{}, 8), queryCount: make(chan string, 8), timeout: timeout}
			h := deliveryHarness(t, conv, clock)
			turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "original input", ClientMessageID: "stall-input"})
			if err != nil {
				t.Fatal(err)
			}
			a, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
			if err != nil || a.Disposition != domain.ChatDeliveryAccepted {
				t.Fatalf("native acknowledgement: %+v %v", a, err)
			}
			clock.awaitTimer(t, 30*time.Second)
			clock.advance(30 * time.Second)
			select {
			case key := <-conv.queryCount:
				if key != a.ID {
					t.Fatal(key)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("original execution was not queried")
			}
			if timeout {
				clock.awaitTimer(t, 10*time.Second)
				clock.advance(10 * time.Second)
			}
			h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
				saved, err := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
				return err == nil && saved.AttentionID != ""
			})
			saved, _ := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
			if saved.Disposition != domain.ChatDeliveryAccepted || saved.EvidenceLevel != 2 || saved.BlockedSince == nil {
				t.Fatalf("accepted evidence downgraded: %+v", saved)
			}
			if timeout && saved.ErrorCode != "CHAT_DELIVERY_QUERY_TIMEOUT" {
				t.Fatal(saved.ErrorCode)
			}
			if len(conv.sentTexts()) != 1 {
				t.Fatal("stalled execution replayed")
			}
			if clock.Now().Sub(a.CreatedAt) > 45*time.Second {
				t.Fatal("attention missed deadline")
			}
		})
	}
}

func TestShutdownPreflightRefusesUnknownWithoutClosingOrAttention(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &stalledDeliveryConversation{fakeConversation: newFakeConversation(), queryCount: make(chan string, 8), timeout: true}
	h := deliveryHarness(t, conv, clock)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "keep original", ClientMessageID: "shutdown-original"})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
	if err = h.svc.StopAll(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("unverified shutdown succeeded: %v", err)
	}
	if !h.svc.HasLiveChatController(testSession) {
		t.Fatal("owner removed before settlement")
	}
	saved, _ := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
	if saved.Disposition != a.Disposition || saved.AttentionID != "" {
		t.Fatalf("shutdown manufactured uncertainty: %+v", saved)
	}
	if len(conv.sentTexts()) != 1 {
		t.Fatal("shutdown replayed input")
	}
}

func TestShutdownFlushesAcceptedReceiptBeforeClosing(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &terminatingConversation{fakeConversation: newFakeConversation()}
	h := deliveryHarness(t, conv, clock)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "native accepted work", ClientMessageID: "flush-before-close"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	var closeChecked bool
	conv.onClose = func() {
		saved, err := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
		if err != nil || saved.Receipt == nil || !saved.Receipt.CustodyFlushed {
			t.Errorf("transport closed before durable custody: %+v %v", saved, err)
		}
		closeChecked = true
	}
	if err = h.svc.StopAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !closeChecked || h.svc.HasLiveChatController(testSession) {
		t.Fatal("successful shutdown did not detach")
	}
	saved, _ := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
	if saved.Disposition != domain.ChatDeliveryAccepted || saved.AttentionID != "" || len(conv.sentTexts()) != 1 {
		t.Fatalf("shutdown changed original execution: %+v", saved)
	}
}

func TestBlockedDeliveryRechecksOriginalOnNativeReady(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &stalledDeliveryConversation{fakeConversation: newFakeConversation(), queryCount: make(chan string, 8)}
	h := deliveryHarness(t, conv, clock)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "one original execution", ClientMessageID: "native-ready-recheck"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	clock.awaitTimer(t, 30*time.Second)
	clock.advance(30 * time.Second)
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		a, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
		return e == nil && a.AttentionID != ""
	})
	conv.fakeDeliveryReceipt(attempt.ID, turn.ProviderTurnID, true)
	// Settlement must clear diagnostics even without a newer progress timestamp.
	conv.mu.Lock()
	receipt := conv.deliveryReceipts[attempt.ID]
	receipt.ObservedAt = attempt.Receipt.ObservedAt
	conv.deliveryReceipts[attempt.ID] = receipt
	conv.mu.Unlock()
	// This event carries no receipt. The monitor must query the original host key.
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		a, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
		return e == nil && a.ExecutionSettled
	})
	saved, err := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
	if err != nil || saved.BlockedSince != nil || saved.ErrorCode != "" {
		t.Fatalf("terminal attempt retained blocked diagnostics: %+v %v", saved, err)
	}
	question, err := h.st.GetAttentionQuestion(t.Context(), testSession, saved.AttentionID)
	if err != nil || question.State == "open" {
		t.Fatalf("settled attempt retained open attention: %+v %v", question, err)
	}
	if len(conv.queryCount) < 2 || len(conv.sentTexts()) != 1 {
		t.Fatal("native readiness did not query original execution, or replayed input")
	}
}

// Embedding the base port deliberately excludes delivery observer methods.
type unmanagedDeliveryConversation struct{ ports.ChatConversation }

func TestUnmanagedConversationDoesNotReserveDelivery(t *testing.T) {
	clock := newSettingsTestClock()
	native := newFakeConversation()
	h := deliveryHarness(t, &unmanagedDeliveryConversation{ChatConversation: native}, clock)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "legacy driver input"})
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := h.st.PendingChatDeliveries(t.Context(), testSession)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("unmanaged driver acquired managed attempt: %+v %v", attempts, err)
	}
	native.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted}
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateCompleted
	})
	clock.advance(time.Minute)
	if err := h.svc.StopAll(t.Context()); err != nil {
		t.Fatalf("idle unmanaged driver cannot stop: %v", err)
	}
}

type deliveryReservationFailureStore struct {
	*store.Store
	failed atomic.Bool
}

func (s *deliveryReservationFailureStore) ReserveChatDelivery(ctx context.Context, a domain.ChatDeliveryAttempt) (domain.ChatDeliveryAttempt, bool, error) {
	if !s.failed.Swap(true) {
		return domain.ChatDeliveryAttempt{}, false, errors.New("injected reservation failure")
	}
	return s.Store.ReserveChatDelivery(ctx, a)
}
func TestReservationFailureRetriesQueuedInputWithoutAnotherSend(t *testing.T) {
	clock := newSettingsTestClock()
	conv := newFakeConversation()
	h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *store.Store) chatsvc.Store { return &deliveryReservationFailureStore{Store: st} }, domain.HarnessCodex, func(o *chatsvc.Options) { o.Now = clock.Now; o.After = clock.After })
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "retry durable intake", ClientMessageID: "reserve-retry"})
	if err != nil || turn.State != domain.TurnStateQueued {
		t.Fatalf("intake lost: %+v %v", turn, err)
	}
	if len(conv.sentTexts()) != 0 {
		t.Fatal("provider received input before reservation")
	}
	clock.awaitTimer(t, time.Second)
	clock.advance(time.Second)
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Turns) == 1 && s.Turns[0].ID == turn.ID && s.Turns[0].State == domain.TurnStateRunning
	})
	if got := conv.sentTexts(); len(got) != 1 || got[0] != "retry durable intake" {
		t.Fatalf("dispatch did not converge once: %v", got)
	}
}

func TestDeliveryProgressRecoveryAllowsFreshAttentionOnNextSilence(t *testing.T) {
	clock := newSettingsTestClock()
	conv := newFakeConversation()
	h := deliveryHarness(t, conv, clock)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "long native work"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	clock.awaitTimer(t, 30*time.Second)
	clock.advance(30 * time.Second)
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		a, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
		return e == nil && a.AttentionID != ""
	})
	blocked, _ := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
	conv.fakeDeliveryReceipt(attempt.ID, turn.ProviderTurnID, false)
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		a, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
		return e == nil && a.BlockedSince == nil && a.AttentionID == ""
	})
	old, err := h.st.GetAttentionQuestion(t.Context(), testSession, blocked.AttentionID)
	if err != nil || old.State == "open" {
		t.Fatalf("recovered progress left old question open: %+v %v", old, err)
	}
	clock.awaitTimer(t, 30*time.Second)
	clock.advance(30 * time.Second)
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		a, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
		return e == nil && a.AttentionID != "" && a.AttentionID != blocked.AttentionID
	})
	blockedAgain, _ := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
	fresh, err := h.st.GetAttentionQuestion(t.Context(), testSession, blockedAgain.AttentionID)
	if err != nil || fresh.State != "open" {
		t.Fatalf("later silence did not create new warning: %+v %v", fresh, err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted})
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		q, e := h.st.GetAttentionQuestion(t.Context(), testSession, fresh.ID)
		return e == nil && q.State != "open"
	})
}

type originalDeliveryReservationStore struct {
	*store.Store
	calls        atomic.Int32
	original     chan domain.ChatDeliveryAttempt
	missingReads atomic.Int32
}

func (s *originalDeliveryReservationStore) ReserveChatDelivery(ctx context.Context, a domain.ChatDeliveryAttempt) (domain.ChatDeliveryAttempt, bool, error) {
	if s.calls.Add(1) == 1 {
		prior := a
		prior.ID = "existing-attempt"
		saved, _, err := s.Store.ReserveChatDelivery(ctx, prior)
		if err != nil {
			return a, false, err
		}
		s.original <- saved
		return a, false, ports.ErrAgentSettingsConflict
	}
	return s.Store.ReserveChatDelivery(ctx, a)
}
func (s *originalDeliveryReservationStore) ChatDeliveryAttempt(ctx context.Context, id domain.SessionID, key string) (domain.ChatDeliveryAttempt, error) {
	a, err := s.Store.ChatDeliveryAttempt(ctx, id, key)
	if err != nil {
		s.missingReads.Add(1)
	}
	return a, err
}
func TestReservationBlockedByOriginalAttemptWaitsForSettlement(t *testing.T) {
	clock := newSettingsTestClock()
	conv := newFakeConversation()
	var fault *originalDeliveryReservationStore
	h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *store.Store) chatsvc.Store {
		fault = &originalDeliveryReservationStore{Store: st, original: make(chan domain.ChatDeliveryAttempt, 1)}
		return fault
	}, domain.HarnessCodex, func(o *chatsvc.Options) { o.Now = clock.Now; o.After = clock.After })
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "wait for exact prior attempt", ClientMessageID: "original-attempt"})
	if err != nil || turn.State != domain.TurnStateQueued {
		t.Fatalf("original intake lost: %+v %v", turn, err)
	}
	prior := <-fault.original
	clock.awaitTimer(t, 30*time.Second)
	clock.advance(60 * time.Second)
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		a, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, prior.ID)
		return e == nil && a.AttentionID != ""
	})
	if fault.calls.Load() != 1 || fault.missingReads.Load() != 1 || len(conv.sentTexts()) != 0 {
		t.Fatalf("blocked input retried/retired nonexistent rows: reserve=%d missingReads=%d sent=%v", fault.calls.Load(), fault.missingReads.Load(), conv.sentTexts())
	}
	// Only native proof of non-delivery permits this original queued input to run.
	conv.mu.Lock()
	conv.deliveryReceipts = map[string]domain.ChatDeliveryReceipt{prior.ID: {AttemptID: prior.ID, ProviderEpoch: "fake-native-epoch", ProviderRequestID: "fake-rpc-" + prior.ID, Disposition: domain.ChatDeliveryNotSent, Source: "not_forwarded", Complete: true, ObservedAt: clock.Now()}}
	conv.mu.Unlock()
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		row, e := h.st.TurnByID(t.Context(), turn.ID)
		return e == nil && row.State == domain.TurnStateRunning && row.ProviderTurnID != ""
	})
	if fault.calls.Load() != 2 || len(conv.sentTexts()) != 1 {
		t.Fatalf("settled original did not wake queue once: calls=%d sent=%v", fault.calls.Load(), conv.sentTexts())
	}
}

type retryDeliveryAckConversation struct {
	*fakeConversation
	ackCalls atomic.Int32
}

func (c *retryDeliveryAckConversation) AcknowledgeDeliveryAttempt(context.Context, ports.ChatDeliveryLookup) error {
	if c.ackCalls.Add(1) == 1 {
		return errors.New("temporary host ACK failure")
	}
	return nil
}
func TestSettledDeliveryAckRetriesWithoutAnotherTurn(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &retryDeliveryAckConversation{fakeConversation: newFakeConversation()}
	h := deliveryHarness(t, conv, clock)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "one settled turn"})
	if err != nil {
		t.Fatal(err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return s.Turns[0].State == domain.TurnStateCompleted })
	clock.awaitTimer(t, 5*time.Second)
	clock.advance(5 * time.Second)
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool { return conv.ackCalls.Load() == 2 })
	if len(conv.sentTexts()) != 1 {
		t.Fatal("ACK retry replayed input")
	}
	if err := h.svc.StopAll(t.Context()); err != nil {
		t.Fatalf("settled ACK recovery blocked shutdown: %v", err)
	}
}
