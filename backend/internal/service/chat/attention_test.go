package chat_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestAttentionRetainsBothProviderSourcesAndSeparatesReadFromPending(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "work", Origin: domain.MessageOriginHuman}); err != nil {
		t.Fatal(err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventApprovalRequested, ProviderTurnID: "provider-turn-1", ProviderItemID: "approval", RequestID: "approval", Summary: "Approve command", Decisions: []ports.ChatDecisionOption{{ID: "allow", Label: "Allow", Kind: ports.ChatDecisionAllowOnce}}})
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventInputRequested, ProviderTurnID: "provider-turn-1", ProviderItemID: "input", RequestID: "input", Input: &ports.ChatInputRequest{Message: "Choose a target"}})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return countActivities(s, domain.ActivityKindApproval) == 1 && countActivities(s, domain.ActivityKindUserInput) == 1
	})
	page, err := h.svc.Attention(ctx, testSession, domain.ConversationAttentionQuery{State: "open", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Counts.Open != 2 || page.Counts.Unread != 2 || !page.HasMoreBefore {
		t.Fatalf("attention = %+v, %v", page, err)
	}
	item := page.Items[0]
	if err := h.svc.MarkAttentionRead(ctx, testSession, page.ActiveBranchID, item.Activity.ID, item.Activity.Revision); err != nil {
		t.Fatal(err)
	}
	read, err := h.svc.Attention(ctx, testSession, domain.ConversationAttentionQuery{State: "open", Limit: 20})
	if err != nil || read.Counts.Open != 2 || read.Counts.Unread != 1 {
		t.Fatalf("mark-read resolved a request = %+v, %v", read, err)
	}
	if err := h.svc.MarkAttentionRead(ctx, testSession, "another-branch", item.Activity.ID, item.Activity.Revision); !errors.Is(err, domain.ErrAttentionChanged) {
		t.Fatalf("wrong branch = %v", err)
	}
}

type attentionSnapshotRaceStore struct {
	*sqlite.Store
	reads int
}

func (s *attentionSnapshotRaceStore) ConversationAttention(context.Context, domain.SessionID, string, domain.ConversationAttentionQuery) (domain.ConversationAttentionPage, error) {
	s.reads++
	return domain.ConversationAttentionPage{ConversationID: "other-conversation", ActiveBranchID: "intervening-branch", Counts: domain.ConversationAttentionCounts{Open: 7}}, nil
}

func TestAttentionRejectsAnInterveningBranchEvenWhenThePageIsEmpty(t *testing.T) {
	var wrapped *attentionSnapshotRaceStore
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		wrapped = &attentionSnapshotRaceStore{Store: st}
		return wrapped
	})
	page, err := h.svc.Attention(context.Background(), testSession, domain.ConversationAttentionQuery{State: "open", Limit: 20})
	if !errors.Is(err, chatsvc.ErrControllerHandoff) || page.Counts.Open != 0 || wrapped.reads != 2 {
		t.Fatalf("accepted mismatched counts/page: %+v, %v, reads=%d", page, err, wrapped.reads)
	}
}
