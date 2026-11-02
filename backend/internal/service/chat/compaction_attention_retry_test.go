package chat_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type transientCompactionAttentionStore struct {
	*store.Store
	fail  atomic.Bool
	calls atomic.Int64
}

func (s *transientCompactionAttentionStore) CreateAttentionQuestion(ctx context.Context, in domain.AttentionQuestionCreate, at time.Time) (domain.AttentionQuestionRecord, bool, error) {
	s.calls.Add(1)
	if s.fail.Load() {
		return domain.AttentionQuestionRecord{}, false, errors.New("transient attention write")
	}
	return s.Store.CreateAttentionQuestion(ctx, in, at)
}

func TestUnknownCompactionReplayRepairsAttentionWithoutDuplicates(t *testing.T) {
	for _, recovery := range []string{"healthy", "replay", "queued-send"} {
		t.Run(recovery, func(t *testing.T) {
			failFirst := recovery != "healthy"
			conv := newCompactingConversation()
			var questions *transientCompactionAttentionStore
			h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *store.Store) chatsvc.Store {
				questions = &transientCompactionAttentionStore{Store: st}
				questions.fail.Store(failFirst)
				return questions
			}, domain.HarnessCodex)
			if _, err := h.ctrl.Compact(t.Context()); err != nil {
				t.Fatal(err)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "compact-native", ProviderEventID: "start"})
			unknown := ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "compact-native", ProviderEventID: "unknown", TurnState: domain.TurnStateFailed, Err: errors.New("ordinary RPC error"), Compaction: &ports.ChatCompactionReceipt{CommandID: "compact-native", Outcome: ports.ChatCompactionUnknown}}
			conv.emit(unknown)
			h.awaitSnapshot(t, func(store.ConversationSnapshot) bool {
				return questions.calls.Load() > 0 && !h.ctrl.SendMuHeldForTest()
			})
			questions.fail.Store(false)
			if recovery == "queued-send" {
				turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "retry diagnosis while remaining queued"})
				if err != nil || turn.State != domain.TurnStateQueued {
					t.Fatalf("queued recovery lost the pending input: %+v %v", turn, err)
				}
				page, err := h.st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{State: "open"}, h.now())
				if err != nil || page.Open != 1 {
					t.Fatalf("queued Send did not repair the failed diagnosis: %+v %v", page, err)
				}
			}
			var questionID string
			for replay := 0; replay < 3; replay++ {
				conv.emit(unknown)
				marker := fmt.Sprintf("after-replay-%d", replay)
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady, ProviderEventID: marker})
				h.awaitSnapshot(t, func(store.ConversationSnapshot) bool {
					events, err := h.st.ProviderEventsSince(t.Context(), h.ctrl.ConversationID(), 0, 100)
					if err != nil {
						return false
					}
					for _, event := range events {
						if event.ProviderEventID == marker {
							return true
						}
					}
					return false
				})
				page, err := h.st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{State: "open"}, h.now())
				if err != nil || page.Open != 1 || questionID != "" && page.Items[0].ID != questionID {
					t.Fatalf("replay did not converge on one attention: %+v %v", page, err)
				}
				questionID = page.Items[0].ID
			}
			turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "keep the original question queued"})
			if err != nil || turn.State != domain.TurnStateQueued || len(conv.sentTexts()) != 0 {
				t.Fatalf("diagnosis repair replayed native work: %+v %v", turn, err)
			}
			wantCalls := int64(1)
			if failFirst {
				wantCalls++
			}
			if questions.calls.Load() != wantCalls {
				t.Fatalf("duplicate attention creation: %d, want %d", questions.calls.Load(), wantCalls)
			}
		})
	}
}
