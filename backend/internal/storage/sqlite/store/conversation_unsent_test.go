package store_test

import (
	"context"
	"testing"
	"time"

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
	if err := s.RequeueUnsentTurn(ctx, conversation.ID, "turn-unsent", "different-id", nil); err == nil {
		t.Fatal("unexpected binding was requeued")
	}
	if err := s.RequeueUnsentTurn(ctx, "different-conversation", "turn-unsent", "prepared-id", nil); err == nil {
		t.Fatal("different conversation was mutated")
	}
	if err := s.RequeueUnsentTurn(ctx, conversation.ID, "turn-unsent", "prepared-id", nil); err != nil {
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
	if err := s.RequeueUnsentTurn(ctx, conversation.ID, "turn-unsent", "", nil); err == nil {
		t.Fatal("terminal turn was reopened")
	}
}

func TestRequeueUnsentBatchResetsSnapshotsAtomically(t *testing.T) {
	ctx := context.Background()
	s, db, rec := settingsStartFixture(t, "")
	current, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"lead", "follower"}
	for _, id := range ids {
		if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, current.OwnerGeneration, domain.ConversationMessage{ID: "message-" + id, Text: id, Origin: domain.MessageOriginHuman}, id, testNow); err != nil {
			t.Fatal(err)
		}
	}
	if batch, err := s.ClaimQueuedBatch(ctx, "start-context", true, 0, testNow); err != nil || len(batch) != 2 {
		t.Fatalf("claim: %+v %v", batch, err)
	}
	frame := dispatchSnapshot(t, current)
	if err := s.RecordTurnSettingsSnapshot(ctx, "start-context", current.OwnerGeneration, ids, frame); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProviderInput(ctx, "lead", "batch input"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindTurnToProvider(ctx, "lead", "prepared", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TRIGGER requeue_fault BEFORE UPDATE ON conversation_turns WHEN NEW.id='follower' BEGIN SELECT RAISE(ABORT,'requeue-injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueUnsentTurn(ctx, "start-context", "lead", "prepared", ids[1:]); err == nil {
		t.Fatal("partial batch requeue accepted")
	}
	lead, err := s.TurnByID(ctx, "lead")
	if err != nil || lead.State != domain.TurnStateRunning || lead.ProviderTurnID != "prepared" || lead.ProviderInputText != "batch input" || lead.AgentSettings == nil {
		t.Fatalf("lead escaped rollback: %+v %v", lead, err)
	}
	var reserved int
	if err := db.QueryRow("SELECT COUNT(*) FROM conversation_turns WHERE id IN ('lead','follower') AND promotion_started_at IS NOT NULL AND agent_settings_snapshot_json<>''").Scan(&reserved); err != nil || reserved != 2 {
		t.Fatalf("partial reservation/snapshot rollback: %d %v", reserved, err)
	}
	if _, err := db.Exec("DROP TRIGGER requeue_fault"); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueUnsentTurn(ctx, "start-context", "lead", "prepared", ids[1:]); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		row, err := s.TurnByID(ctx, id)
		if err != nil || row.State != domain.TurnStateQueued || row.ProviderTurnID != "" || row.AgentSettings != nil || !row.RequestedAt.Equal(testNow) {
			t.Fatalf("unsafe batch requeue: %+v %v", row, err)
		}
	}
	var reservations int
	if err := db.QueryRow("SELECT COUNT(*) FROM conversation_turns WHERE promotion_started_at IS NOT NULL").Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("reserved followers stranded: %d %v", reservations, err)
	}
	later := frame.CapturedAt.Add(time.Second)
	frame.CapturedAt = &later
	if batch, err := s.ClaimQueuedBatch(ctx, "start-context", true, 0, later); err != nil || len(batch) != 2 || batch[0].TurnID != "lead" || batch[1].TurnID != "follower" {
		t.Fatalf("reclaim order: %+v %v", batch, err)
	}
	if err := s.RecordTurnSettingsSnapshot(ctx, "start-context", current.OwnerGeneration, ids, frame); err != nil {
		t.Fatalf("later snapshot conflicts: %v", err)
	}
}
