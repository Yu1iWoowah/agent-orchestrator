package store_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestRequeueUnsentTurnChecksBindingAndPreservesInput(t *testing.T) {
	ctx := context.Background()
	s, session, conversation := seededChatConversation(t)
	appendBranchPrompt(t, s, session, conversation, "generation-root", "unsent", "Original input")
	if err := s.RecordProviderInput(ctx, "turn-unsent", "Rendered provider input"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindTurnToProvider(ctx, "turn-unsent", "prepared-id", testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueUnsentTurn(ctx, conversation.ID, "turn-unsent", "different-id"); err == nil {
		t.Fatal("unexpected binding was requeued")
	}
	if err := s.RequeueUnsentTurn(ctx, "different-conversation", "turn-unsent", "prepared-id"); err == nil {
		t.Fatal("different conversation was mutated")
	}
	if err := s.RequeueUnsentTurn(ctx, conversation.ID, "turn-unsent", "prepared-id"); err != nil {
		t.Fatal(err)
	}
	turn, err := s.TurnByID(ctx, "turn-unsent")
	if err != nil || turn.State != domain.TurnStateQueued || turn.ProviderTurnID != "" || turn.ProviderInputText != "" || turn.StartedAt != nil {
		t.Fatalf("unsafe queue reset: %+v %v", turn, err)
	}
	if err := s.RecordProviderInput(ctx, "turn-unsent", "Original input"); err != nil {
		t.Fatalf("retry cannot record input: %v", err)
	}
	if err := s.SettleTurnByID(ctx, "turn-unsent", domain.TurnStateCompleted, "", testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueUnsentTurn(ctx, conversation.ID, "turn-unsent", ""); err == nil {
		t.Fatal("terminal turn was reopened")
	}
}
