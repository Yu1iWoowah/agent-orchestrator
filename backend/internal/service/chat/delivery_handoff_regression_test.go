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

type blockedUpdateFlushConversation struct {
	*hibernationConversation
	block   atomic.Bool
	entered chan context.Context
}

func (c *blockedUpdateFlushConversation) FenceAndFlushOutbound(ctx context.Context, keys []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	if !c.block.Load() {
		return c.fakeConversation.FenceAndFlushOutbound(ctx, keys)
	}
	c.entered <- ctx
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestUpdateHibernateFlushUsesBoundedContext(t *testing.T) {
	clock := newSettingsTestClock()
	var provider *blockedUpdateFlushConversation
	h, _ := hibernationHarnessWith(t, domain.TurnStateCompleted, func(o *chatsvc.Options) {
		o.HibernationEnabled = func() bool { return true }
		o.After = clock.After
		d := o.Drivers.(fakeRegistry).driver.(fakeDriver)
		provider = &blockedUpdateFlushConversation{hibernationConversation: d.conv.(*hibernationConversation), entered: make(chan context.Context, 1)}
		d.conv = provider
		o.Drivers = fakeRegistry{driver: d}
	})
	provider.block.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := h.svc.HibernateChatForUpdate(ctx, testSession); done <- err }()
	var flushCtx context.Context
	select {
	case flushCtx = <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("flush not reached")
	}
	clock.advance(10 * time.Second)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("flush should refuse normal shutdown")
		}
	case <-time.After(100 * time.Millisecond):
		_, hasDeadline := flushCtx.Deadline()
		cancel()
		<-done
		t.Fatalf("update flush still blocked after 10s budget; provider context hasDeadline=%v", hasDeadline)
	}
}

type postClaimExcerptStore struct {
	*store.Store
	armed, changed atomic.Bool
}

func (s *postClaimExcerptStore) ClaimQueuedBatch(ctx context.Context, cid string, dispatch bool, limit int, at time.Time) ([]domain.QueuedTurn, error) {
	batch, err := s.Store.ClaimQueuedBatch(ctx, cid, dispatch, limit, at)
	if err == nil && len(batch) > 1 && s.armed.CompareAndSwap(true, false) {
		s.changed.Store(true)
	}
	return batch, err
}
func (s *postClaimExcerptStore) ConversationMessages(ctx context.Context, cid string) ([]domain.ConversationMessage, error) {
	messages, err := s.Store.ConversationMessages(ctx, cid)
	if s.changed.Load() {
		for i := range messages {
			if messages[i].Text == "Selected source text." {
				messages[i].Revision++
			}
		}
	}
	return messages, err
}
func TestPostClaimInvalidInputDoesNotFailSibling(t *testing.T) {
	conv := newFakeConversation()
	var fault *postClaimExcerptStore
	h := newHarnessWithConversationAndStore(t, conv, func(st *store.Store) chatsvc.Store { fault = &postClaimExcerptStore{Store: st}; return fault })
	seed, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "seed", ClientMessageID: "audit-seed", Origin: domain.MessageOriginHuman})
	if err != nil {
		t.Fatal(err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventMessageCompleted, ProviderTurnID: seed.ProviderTurnID, ProviderItemID: "source", Text: "Selected source text."}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: seed.ProviderTurnID, TurnState: domain.TurnStateCompleted})
	snap := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return len(s.Messages) == 2 && s.Turns[0].State == domain.TurnStateCompleted
	})
	source := snap.Messages[1]
	first, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "active", ClientMessageID: "audit-active"})
	if err != nil {
		t.Fatal(err)
	}
	bad, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "excerpt changes after claim", ClientMessageID: "audit-bad", Origin: domain.MessageOriginHuman, Excerpts: []ports.ChatExcerptReference{{ConversationID: h.ctrl.ConversationID(), MessageID: source.ID, Revision: source.Revision, Text: "Selected source"}}})
	if err != nil {
		t.Fatal(err)
	}
	good, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "valid sibling", ClientMessageID: "audit-good"})
	if err != nil {
		t.Fatal(err)
	}
	fault.armed.Store(true)
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: first.ProviderTurnID, TurnState: domain.TurnStateCompleted})
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		row, e := h.st.TurnByID(t.Context(), bad.ID)
		return e == nil && row.State == domain.TurnStateFailed
	})
	row, err := h.st.TurnByID(t.Context(), good.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State == domain.TurnStateFailed {
		t.Fatalf("unattempted valid sibling failed: id=%s state=%s provider=%q error=%q", row.ID, row.State, row.ProviderTurnID, row.ErrorMessage)
	}
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		row, e := h.st.TurnByID(t.Context(), good.ID)
		return e == nil && row.State == domain.TurnStateRunning && row.ProviderTurnID != ""
	})
	if got := conv.sentTexts(); len(got) != 3 || got[2] != "valid sibling" {
		t.Fatalf("valid sibling not dispatched once: %v", got)
	}
}

// Transcript and source-turn storage failures are retryable; neither invalidates
// a queued excerpt or changes its original input/attachment receipt.
type transientExcerptStore struct {
	*store.Store
	failMessages, failTurn atomic.Bool
}

func (s *transientExcerptStore) ConversationMessages(ctx context.Context, cid string) ([]domain.ConversationMessage, error) {
	if s.failMessages.Load() {
		return nil, errors.New("temporary excerpt read failure")
	}
	return s.Store.ConversationMessages(ctx, cid)
}
func (s *transientExcerptStore) TurnByID(ctx context.Context, id string) (domain.ConversationTurn, error) {
	if s.failTurn.Load() {
		return domain.ConversationTurn{}, errors.New("temporary source turn read failure")
	}
	return s.Store.TurnByID(ctx, id)
}
func TestQueuedExcerptTemporaryReadFailureRetriesOriginalInput(t *testing.T) {
	for _, failTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "transcript", true: "source-turn"}[failTurn], func(t *testing.T) {
			clock := newSettingsTestClock()
			conv := newFakeConversation()
			var fault *transientExcerptStore
			h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *store.Store) chatsvc.Store { fault = &transientExcerptStore{Store: st}; return fault }, domain.HarnessCodex, func(o *chatsvc.Options) { o.Now = clock.Now; o.After = clock.After })
			seed, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "seed", ClientMessageID: "excerpt-source"})
			if err != nil {
				t.Fatal(err)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventMessageCompleted, ProviderTurnID: seed.ProviderTurnID, ProviderItemID: "source", Text: "Selected source text."}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: seed.ProviderTurnID, TurnState: domain.TurnStateCompleted})
			snap := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return len(s.Messages) == 2 && s.Turns[0].State == domain.TurnStateCompleted
			})
			source := snap.Messages[1]
			active, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "active"})
			if err != nil {
				t.Fatal(err)
			}
			queued, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "keep original excerpt", ClientMessageID: "excerpt-retry", Origin: domain.MessageOriginHuman, Excerpts: []ports.ChatExcerptReference{{ConversationID: h.ctrl.ConversationID(), MessageID: source.ID, Revision: source.Revision, Text: "Selected source"}}})
			if err != nil {
				t.Fatal(err)
			}
			original, err := h.st.ListQueuedBatch(t.Context(), h.ctrl.ConversationID(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if failTurn {
				fault.failTurn.Store(true)
			} else {
				fault.failMessages.Store(true)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: active.ProviderTurnID, TurnState: domain.TurnStateCompleted})
			clock.awaitTimer(t, time.Second)
			kept, err := h.st.ListQueuedBatch(t.Context(), h.ctrl.ConversationID(), 0)
			if err != nil || len(kept) != 1 || kept[0].TurnID != queued.ID || kept[0].DeliveryContentJSON != original[0].DeliveryContentJSON || kept[0].ClientMessageID != "excerpt-retry" {
				t.Fatalf("temporary read lost intake: %+v %v", kept, err)
			}
			if len(conv.sentTexts()) != 2 {
				t.Fatal("temporary failure forwarded input")
			}
			fault.failMessages.Store(false)
			fault.failTurn.Store(false)
			clock.advance(time.Second)
			h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
				row, e := h.st.TurnByID(t.Context(), queued.ID)
				return e == nil && row.State == domain.TurnStateRunning && row.ProviderTurnID != ""
			})
			if len(conv.sentTexts()) != 3 {
				t.Fatalf("original input not retried once: %v", conv.sentTexts())
			}
		})
	}
}
