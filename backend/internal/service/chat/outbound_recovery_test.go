package chat_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type reconcilingOutboundConversation struct {
	*fakeConversation
	idle             atomic.Bool
	reconcileStarted chan struct{}
}

func (c *reconcilingOutboundConversation) ReconcileOutbound(ctx context.Context) error {
	if c.reconcileStarted != nil {
		c.reconcileStarted <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	if !c.idle.Load() {
		return ports.ErrChatShutdownNotSettled
	}
	return nil
}
func (c *reconcilingOutboundConversation) FenceAndFlushOutbound(ctx context.Context, keys []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	if !c.idle.Load() {
		return nil, ports.ErrChatShutdownNotSettled
	}
	return c.fakeConversation.FenceAndFlushOutbound(ctx, keys)
}
func TestShutdownUnknownOutboundHasAttentionAndNativeRecovery(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &reconcilingOutboundConversation{fakeConversation: newFakeConversation()}
	h := deliveryHarness(t, conv, clock)
	// No canonical input is submitted; only uncovered native work blocks shutdown.
	if _, err := h.svc.Snapshot(t.Context(), testSession); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.StopAll(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("unknown detached: %v", err)
	}
	page, err := h.st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, clock.Now())
	if err != nil || page.Open != 1 {
		t.Fatalf("unknown shutdown invisible: %+v %v preflight=%v", page, err, h.svc.PreflightShutdown(t.Context()))
	}
	// Repeating the unsuccessful preflight does not create another question.
	_ = h.svc.StopAll(t.Context())
	page, err = h.st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, clock.Now())
	if err != nil || page.Open != 1 {
		t.Fatalf("duplicate attention: %+v %v", page, err)
	}
	conv.idle.Store(true)
	if err := h.svc.StopAll(t.Context()); err != nil {
		t.Fatalf("native idle recovery still fenced: %v", err)
	}
	page, err = h.st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, clock.Now())
	if err != nil || page.Open != 0 {
		t.Fatalf("settled attention remained open: %+v %v", page, err)
	}
}

func TestExplicitSessionStopWithdrawsUnknownOutboundAttention(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &reconcilingOutboundConversation{fakeConversation: newFakeConversation()}
	h := deliveryHarness(t, conv, clock)
	if err := h.svc.PreflightShutdown(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatal(err)
	}
	page, err := h.st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, clock.Now())
	if err != nil || page.Open != 1 {
		t.Fatalf("missing reason: %+v %v", page, err)
	}
	if err := h.svc.Stop(t.Context(), testSession); err != nil {
		t.Fatalf("explicit stop unavailable: %v", err)
	}
	page, err = h.st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, clock.Now())
	if err != nil || page.Open != 0 {
		t.Fatalf("explicitly stopped work still pending: %+v %v", page, err)
	}
}

func TestReviewerUnknownOutboundHasAttentionAndExplicitStop(t *testing.T) {
	st := openStore(t)
	at := time.Now().UTC()
	reviewID := "outbound-review"
	if err := st.UpsertReview(t.Context(), domain.Review{ID: reviewID, SessionID: testSession, ProjectID: testProject, Harness: domain.ReviewerCodex, InterfaceMode: domain.ReviewerInterfaceChat, CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	worker := &reconcilingOutboundConversation{fakeConversation: newFakeConversation()}
	reviewer := &reconcilingOutboundConversation{fakeConversation: newFakeConversation()}
	// Distinct provider hosts may assign the same native ID. AO owners must
	// retain separate questions and separate explicit termination authority.
	worker.providerConversationID = "same-native-id"
	reviewer.providerConversationID = "same-native-id"
	svc := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Reader: fullSnapshotReader(st), Drivers: fakeRegistry{driver: fakeDriver{start: func(cfg ports.ChatStartConfig) (ports.ChatConversation, error) {
		if cfg.SessionID == testSession {
			return worker, nil
		}
		return reviewer, nil
	}}}, NewID: uuid.NewString})
	owner := domain.ReviewConversationOwner(reviewID)
	t.Cleanup(func() {
		_ = svc.StopForOwner(context.Background(), owner)
		_ = svc.Stop(context.Background(), testSession)
	})
	for _, target := range []domain.ConversationOwner{domain.SessionConversationOwner(testSession), owner} {
		controller, err := svc.Start(t.Context(), chatsvc.StartConfig{Owner: target, SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if err := controller.PreflightShutdown(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
			t.Fatal(err)
		}
	}
	page, err := st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, time.Now().UTC())
	if err != nil || page.Open != 2 {
		t.Fatalf("reviewer and worker reason mixed: %+v %v", page, err)
	}
	if err := svc.StopForOwner(t.Context(), owner); err != nil {
		t.Fatalf("explicit reviewer stop unavailable: %v", err)
	}
	page, err = st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, time.Now().UTC())
	if err != nil || page.Open != 1 || !svc.HasLiveChatController(testSession) {
		t.Fatalf("stopping reviewer removed worker reason: %+v %v", page, err)
	}
	if err := svc.Stop(t.Context(), testSession); err != nil {
		t.Fatal(err)
	}
	page, err = st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, time.Now().UTC())
	if err != nil || page.Open != 0 {
		t.Fatalf("stopped worker still pending: %+v %v", page, err)
	}
}

func TestShutdownUnknownOutboundAttentionPrecedesNativeReadTimeout(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &reconcilingOutboundConversation{fakeConversation: newFakeConversation(), reconcileStarted: make(chan struct{}, 1)}
	h := deliveryHarness(t, conv, clock)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.svc.PreflightShutdown(ctx) }()
	select {
	case <-conv.reconcileStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("native reconciliation did not start")
	}
	defer clock.advance(10 * time.Second)
	page, err := h.st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, clock.Now())
	if err != nil || page.Open != 1 {
		t.Fatalf("native query consumed attention budget: %+v %v", page, err)
	}
	clock.advance(10 * time.Second)
	select {
	case err := <-done:
		if !errors.Is(err, ports.ErrChatShutdownNotSettled) {
			t.Fatalf("query timeout detached provider: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("preflight exceeded its deadline")
	}
}

type failingAttentionCancelStore struct {
	*store.Store
	cancellations atomic.Int64
}

func (s *failingAttentionCancelStore) CancelAttentionQuestion(context.Context, domain.SessionID, string, int64, string, string, time.Time) (domain.AttentionQuestionRecord, error) {
	s.cancellations.Add(1)
	return domain.AttentionQuestionRecord{}, errors.New("attention cancellation unavailable")
}
func TestExplicitStopSucceedsWhenAttentionCleanupFails(t *testing.T) {
	conv := &reconcilingOutboundConversation{fakeConversation: newFakeConversation()}
	var failed *failingAttentionCancelStore
	h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *store.Store) chatsvc.Store { failed = &failingAttentionCancelStore{Store: st}; return failed }, domain.HarnessCodex)
	if err := h.svc.PreflightShutdown(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatal(err)
	}
	var closed atomic.Bool
	conv.onClose = func() { closed.Store(true) }
	if err := h.svc.Stop(t.Context(), testSession); err != nil {
		t.Fatalf("native stop succeeded but diagnostic cleanup returned failure: %v", err)
	}
	if !closed.Load() || failed.cancellations.Load() == 0 {
		t.Fatal("test did not exercise successful native stop plus failed attention cleanup")
	}
}

func (c *reconcilingOutboundConversation) OutboundProviderEpoch() string { return "fake-native-epoch" }

type closeReplyLostConversation struct {
	*reconcilingOutboundConversation
}

func (c *closeReplyLostConversation) Close() error {
	_ = c.fakeConversation.Close()
	return errors.New("shutdown reply lost after stream closed")
}

func TestStopRetryWithdrawsOriginalOutboundAttention(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &closeReplyLostConversation{&reconcilingOutboundConversation{fakeConversation: newFakeConversation()}}
	var hostStops int
	h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *store.Store) chatsvc.Store { return st }, domain.HarnessCodex, func(o *chatsvc.Options) {
		o.Now, o.After = clock.Now, clock.After
		o.StopProviderHost = func(context.Context, domain.SessionID) error { hostStops++; return nil }
	})
	if err := h.svc.PreflightShutdown(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatal(err)
	}
	page, err := h.st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, clock.Now())
	if err != nil || page.Open != 1 {
		t.Fatalf("missing attention: %+v %v", page, err)
	}
	if err := h.svc.Stop(t.Context(), testSession); err == nil {
		t.Fatal("first close must report the lost reply")
	}
	if err := h.svc.Stop(t.Context(), testSession); err != nil {
		t.Fatalf("explicit retry failed: %v", err)
	}
	if hostStops == 0 {
		t.Fatal("retry did not confirm host stop")
	}
	page, err = h.st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{}, clock.Now())
	if err != nil || page.Open != 0 {
		t.Fatalf("successful stop left stale attention: %+v %v", page, err)
	}
}
