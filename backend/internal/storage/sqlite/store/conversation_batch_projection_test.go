package store_test

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// This real SQLite snapshot is also the desktop/mobile renderer regression fixture.
// Regenerate with B4_FIXTURE_OUT set to test/fixtures/batch-conversation.json.
func TestBatchConversationProjection(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	for i, id := range []string{"leader", "follower"} {
		created, err := s.AppendUserMessage(ctx, conversation, session, "gen-1", domain.ConversationMessage{
			ID: id + "-message", Text: "Batch " + id, ClientMessageID: id, Origin: domain.MessageOriginHuman,
		}, id, histClock.Add(time.Duration(i)*time.Second))
		if err != nil || !created {
			t.Fatalf("append %s: created=%v err=%v", id, created, err)
		}
	}
	batch, err := s.ClaimQueuedBatch(ctx, conversation, true, 0, histClock.Add(time.Minute))
	if err != nil || len(batch) != 2 {
		t.Fatalf("claim: %+v, %v", batch, err)
	}
	if err := s.BindTurnToProvider(ctx, "leader", "provider-batch", histClock.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteQueuedBatch(ctx, conversation, []string{"follower"}, "provider-batch", histClock.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleAssistantMessage(ctx, conversation, "provider-answer", "provider-batch", "Shared batch answer", "answer", histClock.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleTurn(ctx, conversation, "provider-batch", domain.TurnStateCompleted, "", histClock.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.LoadConversationSnapshot(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		Turns    []domain.ConversationTurn    `json:"turns"`
		Messages []domain.ConversationMessage `json:"messages"`
	}{snapshot.Turns, snapshot.Messages}
	data, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("B4_FIXTURE_OUT"); path != "" {
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the checked-in renderer input tied to this real database behavior.
	if os.Getenv("B4_FIXTURE_OUT") == "" {
		golden, err := os.ReadFile("../../../../../test/fixtures/batch-conversation.json")
		if err != nil {
			t.Fatal(err)
		}
		if string(golden) != string(append(data, '\n')) {
			t.Fatal("renderer fixture drift; regenerate with B4_FIXTURE_OUT")
		}
	}

	if got := texts(snapshot.Messages); !reflect.DeepEqual(got, []string{"Batch leader", "Batch follower", "Shared batch answer"}) {
		t.Fatalf("messages: %v", got)
	}
	if got := turnIDs(snapshot.Turns); !reflect.DeepEqual(got, []string{"leader"}) {
		t.Fatalf("visible turns: %v", got)
	}
	check := func(t *testing.T, messages []domain.ConversationMessage) {
		t.Helper()
		for _, message := range messages {
			raw, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if message.ID == "follower-message" {
				if message.TurnID != "follower" || fields["consumedByTurnId"] != "leader" {
					t.Errorf("batch follower lost original identity or consuming turn: %s", raw)
				}
			} else if fields["consumedByTurnId"] != nil {
				t.Errorf("non-follower has consuming turn: %s", raw)
			}
		}
	}
	t.Run("full", func(t *testing.T) { check(t, snapshot.Messages) })
	t.Run("transcript", func(t *testing.T) {
		messages, err := s.ConversationMessages(ctx, conversation)
		if err != nil {
			t.Fatal(err)
		}
		check(t, messages)
	})
	t.Run("pages", func(t *testing.T) {
		var messages []domain.ConversationMessage
		before := int64(0)
		for {
			page, err := s.LoadConversationSnapshotPage(ctx, conversation, before, 1)
			if err != nil {
				t.Fatal(err)
			}
			messages = append(page.Messages, messages...)
			if !page.HasMoreBefore {
				break
			}
			if page.OldestSequence == before {
				t.Fatal("pagination stalled")
			}
			before = page.OldestSequence
		}
		if !reflect.DeepEqual(messages, snapshot.Messages) {
			t.Fatalf("paged messages differ: %+v", messages)
		}
		check(t, messages)
	})
	t.Run("branch lineage", func(t *testing.T) {
		root := snapshot.Conversation.ActiveBranchID
		child := domain.ConversationBranch{
			ID: "batch-child", ConversationID: conversation, SessionID: session,
			ParentBranchID: root, ProviderConversationID: "child-thread",
			ForkAfterTurnID: "leader", ForkAfterSequence: 3,
		}
		if err := s.CreateConversationBranch(ctx, child, histClock.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.ActivateConversationBranch(ctx, session, conversation, child.ID, "child-thread", "gen-child", histClock.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := s.ActivateConversationBranch(ctx, session, conversation, root, "", "gen-1", histClock.Add(4*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}()
		for _, load := range []func() (store.ConversationSnapshot, error){
			func() (store.ConversationSnapshot, error) { return s.LoadConversationSnapshot(ctx, conversation) },
			func() (store.ConversationSnapshot, error) {
				return s.LoadConversationSnapshotPage(ctx, conversation, 0, 10)
			},
		} {
			got, err := load()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Messages, snapshot.Messages) {
				t.Fatalf("branch lost batch history: %+v", got.Messages)
			}
			check(t, got.Messages)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		if _, err := s.RollbackTurns(ctx, conversation, "leader", histClock.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
		for _, load := range []func() (store.ConversationSnapshot, error){
			func() (store.ConversationSnapshot, error) { return s.LoadConversationSnapshot(ctx, conversation) },
			func() (store.ConversationSnapshot, error) {
				return s.LoadConversationSnapshotPage(ctx, conversation, 0, 10)
			},
		} {
			got, err := load()
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Messages) != 0 {
				t.Fatalf("rolled-back batch visible: %+v", got.Messages)
			}
		}
	})
}
