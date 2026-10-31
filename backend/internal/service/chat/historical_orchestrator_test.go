package chat_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestRetiredOrchestratorSnapshotKeepsItsOwnTranscript(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	st, oldID := openProvisioningStore(t, domain.SessionProvisionReady)
	old, _, err := st.GetSession(ctx, oldID)
	if err != nil {
		t.Fatal(err)
	}
	old.Kind = domain.KindOrchestrator
	old.Metadata.ControllerGeneration = "old-generation"
	old.Metadata.ProviderConversationID = "old-native"
	if err := st.UpdateSession(ctx, old); err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation(ctx, "history-project", domain.ConversationScopeProject, testProject, oldID, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		_, err := st.AppendUserMessage(ctx, conv.ID, oldID, "old-generation", domain.ConversationMessage{ID: fmt.Sprintf("old-message-%d", i), Origin: domain.MessageOriginHuman, Text: fmt.Sprintf("old prompt %d", i)}, fmt.Sprintf("old-turn-%d", i), now.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	conv, err = st.ConversationForSession(ctx, oldID)
	if err != nil {
		t.Fatal(err)
	}
	old.IsTerminated = true
	if err := st.UpdateSession(ctx, old); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, ProvisionState: domain.SessionProvisionReady, CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.CreateProjectConversationWithContextReset(ctx, "unused", testProject, fresh.ID, domain.ConversationActivity{ID: "reset-new", ProviderItemID: domain.ConversationContextResetProviderItemID(fresh.ID), Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	branch := domain.ConversationBranch{ID: "new-branch", ConversationID: conv.ID, SessionID: fresh.ID, ParentBranchID: conv.ActiveBranchID, ForkAfterSequence: conv.LatestSequence, ProviderConversationID: "new-native", CreatedAt: now.Add(time.Minute)}
	if err := st.CreateAndActivateConversationBranch(ctx, fresh.ID, branch, "new-generation", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendUserMessage(ctx, conv.ID, fresh.ID, "new-generation", domain.ConversationMessage{ID: "new-message", Origin: domain.MessageOriginHuman, Text: "successor prompt"}, "new-turn", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	svc := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Reader: fullSnapshotReader(st), PageReader: boundedSnapshotReader(st), HistoricalReader: func(ctx context.Context, session domain.SessionID, before, limit int64) (chatsvc.ConversationRows, error) {
		rows, err := st.LoadHistoricalSessionSnapshotPage(ctx, session, before, limit)
		return chatsvc.ConversationRows{Conversation: rows.Conversation, ActiveBranch: rows.ActiveBranch, Turns: rows.Turns, Messages: rows.Messages, Activities: rows.Activities, OldestSequence: rows.OldestSequence, HasMoreBefore: rows.HasMoreBefore}, err
	},
	})
	before, err := st.ConversationForSession(ctx, fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := svc.SnapshotPage(ctx, oldID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Activities) != 0 {
		t.Fatalf("history includes a successor reset: %+v", snapshot.Activities)
	}
	if snapshot.Controller != "stopped" || snapshot.NativeForkAvailableAfterSequence != 0 || len(snapshot.BranchPoints) != 0 {
		t.Fatalf("historical snapshot exposes live controls: %+v", snapshot)
	}
	for _, message := range snapshot.Messages {
		if message.Sequence > snapshot.EditFloorSequence {
			t.Fatal("historical message can be edited")
		}
	}
	for _, turn := range snapshot.Turns {
		if turn.ProviderTurnID != "" {
			t.Fatal("historical snapshot exposes provider turn controls")
		}
	}
	if len(snapshot.Messages) != 2 {
		t.Fatalf("old transcript has %d messages, want a page of 2 retained messages", len(snapshot.Messages))
	}
	if snapshot.Messages[0].Text != "old prompt 1" || snapshot.Messages[1].Text != "old prompt 2" || !snapshot.HasMoreBefore {
		t.Fatalf("old page = %+v", snapshot)
	}
	older, err := svc.SnapshotPage(ctx, oldID, snapshot.OldestSequence, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Messages) != 1 || older.Messages[0].Text != "old prompt 0" || older.HasMoreBefore {
		t.Fatalf("older page = %+v", older)
	}
	after, err := st.ConversationForSession(ctx, fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.SessionID != after.SessionID || before.ActiveBranchID != after.ActiveBranchID {
		t.Fatal("historical read changed current ownership")
	}
	if _, err := st.LoadHistoricalSessionSnapshotPage(ctx, fresh.ID, 0, 2); !errors.Is(err, domain.ErrNoConversation) {
		t.Fatalf("live orchestrator admitted as historical: %v", err)
	}
	whole, err := svc.Snapshot(ctx, oldID)
	if err != nil || len(whole.Messages) != 3 {
		t.Fatalf("full historical snapshot: messages=%d err=%v", len(whole.Messages), err)
	}
	current, err := svc.SnapshotPage(ctx, fresh.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Messages) != 1 || current.Messages[0].Text != "successor prompt" {
		t.Fatalf("current history = %+v", current.Messages)
	}
}
