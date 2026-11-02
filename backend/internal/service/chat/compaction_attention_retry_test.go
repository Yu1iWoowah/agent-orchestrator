package chat_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

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

func TestReviewerUnknownCompactionRepairRequiresCurrentOwner(t *testing.T) {
	for _, replay := range []bool{false, true} {
		for _, ownership := range []string{"current", "replaced", "terminal-mode"} {
			t.Run(fmt.Sprintf("%s/replay=%t", ownership, replay), func(t *testing.T) {
				st := openStore(t)
				at := time.Now()
				const reviewID = "compaction-review"
				if err := st.UpsertReview(t.Context(), domain.Review{ID: reviewID, SessionID: testSession, ProjectID: testProject, Harness: domain.ReviewerOpenCode, InterfaceMode: domain.ReviewerInterfaceChat, CreatedAt: at, UpdatedAt: at}); err != nil {
					t.Fatal(err)
				}
				conv := newCompactingConversation()
				conv.events = make(chan ports.ChatEvent)
				questions := &transientCompactionAttentionStore{Store: st}
				questions.fail.Store(replay)
				svc := chatsvc.New(chatsvc.Options{Store: questions, Sessions: st, Reader: fullSnapshotReader(st), Drivers: fakeRegistry{driver: fakeDriver{conv: conv}}, NewID: uuid.NewString, Log: slog.New(slog.DiscardHandler)})
				owner := domain.ReviewConversationOwner(reviewID)
				t.Cleanup(func() { _ = svc.StopForOwner(context.Background(), owner) })
				ctrl, err := svc.Start(t.Context(), chatsvc.StartConfig{Owner: owner, SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessOpenCode, WorkspacePath: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ctrl.Compact(t.Context()); err != nil {
					t.Fatal(err)
				}
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "review-compact", ProviderEventID: "start"})
				awaitStoreSnapshot(t, st, ctrl.ConversationID(), func(s store.ConversationSnapshot) bool {
					return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateRunning && !ctrl.SendMuHeldForTest()
				})
				unknown := ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "review-compact", ProviderEventID: "unknown", TurnState: domain.TurnStateFailed, Err: errors.New("unknown RPC outcome"), Compaction: &ports.ChatCompactionReceipt{CommandID: "review-compact", Outcome: ports.ChatCompactionUnknown}}
				var wantCalls int64
				if replay {
					conv.emit(unknown)
					conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})
					if questions.calls.Load() != 1 {
						t.Fatalf("first diagnosis was not attempted: %d", questions.calls.Load())
					}
					wantCalls = 1
					questions.fail.Store(false)
				}
				switch ownership {
				case "replaced":
					if ok, err := st.ClaimReviewChatController(t.Context(), reviewID, conv.ProviderConversationID(), "replacement-generation", time.Now()); err != nil || !ok {
						t.Fatalf("takeover: %v %v", ok, err)
					}
				case "terminal-mode":
					review, found, err := st.GetReviewByID(t.Context(), reviewID)
					if err != nil || !found {
						t.Fatalf("read original owner: %v %v", found, err)
					}
					review.InterfaceMode = domain.ReviewerInterfaceTUI
					if err := st.UpsertReview(t.Context(), review); err != nil {
						t.Fatal(err)
					}
				}
				wantOpen := int64(0)
				if ownership == "current" {
					wantOpen = 1
					wantCalls++
				}
				var questionID string
				for attempt := 0; attempt < 3; attempt++ {
					conv.emit(unknown)
					// The next unbuffered event follows completion of unknown projection.
					conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})
					page, err := st.ListAttentionQuestions(t.Context(), testSession, domain.AttentionQuestionQuery{State: "open"}, time.Now())
					if err != nil || page.Open != wantOpen || questions.calls.Load() != wantCalls {
						t.Fatalf("repair crossed ownership: open=%d calls=%d want=%d/%d err=%v", page.Open, questions.calls.Load(), wantOpen, wantCalls, err)
					}
					if wantOpen == 1 {
						if questionID != "" && page.Items[0].ID != questionID {
							t.Fatal("replay replaced the existing question")
						}
						questionID = page.Items[0].ID
					}
				}
				events, err := st.ProviderEventsSince(t.Context(), ctrl.ConversationID(), 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				archived := false
				for _, event := range events {
					archived = archived || event.ProviderEventID == unknown.ProviderEventID
				}
				if archived != (replay || ownership == "current") || len(conv.sentTexts()) != 0 {
					t.Fatalf("repair changed archive ownership or sent native work: archived=%v sent=%v", archived, conv.sentTexts())
				}
			})
		}
	}
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
