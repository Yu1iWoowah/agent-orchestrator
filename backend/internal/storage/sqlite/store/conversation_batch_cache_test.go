package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Keep the real 200-message page boundaries and turn records, but omit the 501
// unrelated output bodies from the shared renderer fixture.
func TestBatchConversationCachedHistory(t *testing.T) {
	ctx := context.Background()
	s, session, conversation := conversationFixture(t)
	appendPrompt := func(id string) {
		t.Helper()
		created, err := s.AppendUserMessage(ctx, conversation, session, "gen-1", domain.ConversationMessage{
			ID: id + "-message", Text: id, Origin: domain.MessageOriginHuman, ClientMessageID: id,
		}, id, histClock)
		if err != nil || !created {
			t.Fatalf("append %s: %v %v", id, created, err)
		}
	}
	appendPrompt("active")
	if _, err := s.ClaimQueuedBatch(ctx, conversation, true, 1, histClock); err != nil {
		t.Fatal(err)
	}
	if err := s.BindTurnToProvider(ctx, "active", "provider-active", histClock); err != nil {
		t.Fatal(err)
	}
	appendPrompt("leader")
	appendPrompt("follower")
	for i := 0; i < 501; i++ {
		id := fmt.Sprintf("output-%d", i)
		if err := s.SettleAssistantMessage(ctx, conversation, id, "provider-active", id, id, histClock); err != nil {
			t.Fatal(err)
		}
	}
	type pageFixture struct {
		Turns          []domain.ConversationTurn    `json:"turns"`
		Messages       []domain.ConversationMessage `json:"messages"`
		OldestSequence int64                        `json:"oldestSequence"`
		LatestSequence int64                        `json:"latestSequence"`
		HasMoreBefore  bool                         `json:"hasMoreBefore"`
	}
	compact := func(snapshot store.ConversationSnapshot) pageFixture {
		messages := []domain.ConversationMessage{}
		for _, m := range snapshot.Messages {
			if m.Role == domain.MessageRoleUser || m.ID == "answer" {
				messages = append(messages, m)
			}
		}
		return pageFixture{snapshot.Turns, messages, snapshot.OldestSequence, snapshot.Conversation.LatestSequence, snapshot.HasMoreBefore}
	}
	var before []pageFixture
	cursor := int64(0)
	for {
		page, err := s.LoadConversationSnapshotPage(ctx, conversation, cursor, 200)
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, compact(page))
		if !page.HasMoreBefore {
			break
		}
		if page.OldestSequence == cursor {
			t.Fatal("pagination stalled")
		}
		cursor = page.OldestSequence
	}
	if len(before) < 2 {
		t.Fatal("fixture did not span historical pages")
	}
	if err := s.SettleTurn(ctx, conversation, "provider-active", domain.TurnStateCompleted, "", histClock); err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimQueuedBatch(ctx, conversation, true, 0, histClock.Add(time.Minute))
	if err != nil || len(batch) != 2 {
		t.Fatalf("batch: %+v, %v", batch, err)
	}
	if err := s.BindTurnToProvider(ctx, "leader", "provider-batch", histClock); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteQueuedBatch(ctx, conversation, []string{"follower"}, "provider-batch", histClock); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleAssistantMessage(ctx, conversation, "provider-answer", "provider-batch", "Shared batch answer", "answer", histClock); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleTurn(ctx, conversation, "provider-batch", domain.TurnStateCompleted, "", histClock); err != nil {
		t.Fatal(err)
	}
	live, err := s.LoadConversationSnapshotPage(ctx, conversation, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Messages) == 0 || len(live.Messages) > 200 || !live.HasMoreBefore {
		t.Fatalf("live page is not bounded: %d, %v", len(live.Messages), live.HasMoreBefore)
	}
	for _, m := range live.Messages {
		if m.TurnID == "follower" || m.ID == "leader-message" {
			t.Fatalf("fixture unexpectedly loaded batch input: %+v", m)
		}
	}
	full, err := s.LoadConversationSnapshot(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		Before []pageFixture `json:"before"`
		Live   pageFixture   `json:"live"`
		Full   pageFixture   `json:"full"`
	}{before, compact(live), compact(full)}
	data, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("B4_CACHE_FIXTURE_OUT"); path != "" {
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		golden, err := os.ReadFile("../../../../../test/fixtures/batch-conversation-pages.json")
		if err != nil {
			t.Fatal(err)
		}
		if string(golden) != string(append(data, '\n')) {
			t.Fatal("cached page fixture drift; regenerate with B4_CACHE_FIXTURE_OUT")
		}
	}
	for _, snapshot := range []store.ConversationSnapshot{live, full} {
		for _, turn := range snapshot.Turns {
			if turn.ID != "leader" {
				continue
			}
			raw, err := json.Marshal(turn)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			inputs, ok := fields["batchInputTurnIds"].([]any)
			if !ok || len(inputs) != 1 || inputs[0] != "follower" {
				t.Errorf("consuming turn lacks off-page batch receipt: %s", raw)
			}
		}
	}
}
