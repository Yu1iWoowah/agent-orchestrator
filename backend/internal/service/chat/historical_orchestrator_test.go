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

func TestRetiredOrchestratorSnapshotOmitsAdoptedSuccessorLiveTurn(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateQueued, domain.TurnStateRunning} {
		t.Run(string(state), func(t *testing.T) {
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
			conv, err := st.CreateConversation(ctx, "first-project-chat", domain.ConversationScopeProject, testProject, oldID, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AppendUserMessage(ctx, conv.ID, oldID, "old-generation", domain.ConversationMessage{ID: "old-message", Origin: domain.MessageOriginHuman, Text: "source prompt"}, "old-turn", now); err != nil {
				t.Fatal(err)
			}
			if err := st.SettleTurnByID(ctx, "old-turn", domain.TurnStateCompleted, "", now); err != nil {
				t.Fatal(err)
			}
			old.IsTerminated = true
			if err := st.UpdateSession(ctx, old); err != nil {
				t.Fatal(err)
			}
			fresh, err := st.CreateSession(ctx, domain.SessionRecord{
				ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode,
				Mode: domain.SessionModeChat, ProvisionState: domain.SessionProvisionProvisioning,
				CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			queue, err := st.CreateConversation(ctx, "provisioning-chat", domain.ConversationScopeSession, testProject, fresh.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AppendUserMessage(ctx, queue.ID, fresh.ID, "", domain.ConversationMessage{ID: "successor-message", Origin: domain.MessageOriginHuman, Text: "successor prompt"}, "successor-turn", now); err != nil {
				t.Fatal(err)
			}
			adopted, err := st.CreateProjectConversationAdoptingQueue(ctx, "unused", testProject, fresh.ID, domain.ConversationActivity{
				ID: "successor-reset", ProviderItemID: domain.ConversationContextResetProviderItemID(fresh.ID),
				Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted,
			}, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if err := st.CreateAndActivateConversationBranch(ctx, fresh.ID, domain.ConversationBranch{
				ID: "successor-branch", ConversationID: conv.ID, SessionID: fresh.ID,
				ParentBranchID: conv.ActiveBranchID, ForkAfterSequence: adopted.LatestSequence,
				ProviderConversationID: "successor-native", CreatedAt: now.Add(time.Minute),
			}, "successor-generation", now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if state == domain.TurnStateRunning {
				batch, err := st.ClaimQueuedBatch(ctx, conv.ID, true, 1, now.Add(time.Minute))
				if err != nil || len(batch) != 1 || batch[0].TurnID != "successor-turn" {
					t.Fatalf("claim successor: batch=%+v err=%v", batch, err)
				}
			}
			current, err := st.LoadConversationSnapshotPage(ctx, conv.ID, 0, 10)
			if err != nil || len(current.Turns) != 1 || current.Turns[0].ID != "successor-turn" || current.Turns[0].BranchID != conv.ActiveBranchID || current.Turns[0].State != state {
				t.Fatalf("successor must remain on the source root branch: turns=%+v err=%v", current.Turns, err)
			}
			svc := chatsvc.New(chatsvc.Options{
				Store: st, Sessions: st, Reader: fullSnapshotReader(st), PageReader: boundedSnapshotReader(st),
				HistoricalReader: func(ctx context.Context, id domain.SessionID, before, limit int64) (chatsvc.ConversationRows, error) {
					rows, err := st.LoadHistoricalSessionSnapshotPage(ctx, id, before, limit)
					return chatsvc.ConversationRows{Conversation: rows.Conversation, ActiveBranch: rows.ActiveBranch, Turns: rows.Turns, Messages: rows.Messages, Activities: rows.Activities, OldestSequence: rows.OldestSequence, HasMoreBefore: rows.HasMoreBefore}, err
				},
			})
			for _, full := range []bool{false, true} {
				var snapshot chatsvc.Snapshot
				if full {
					snapshot, err = svc.Snapshot(ctx, oldID)
				} else {
					snapshot, err = svc.SnapshotPage(ctx, oldID, 0, 10)
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Messages) != 1 || snapshot.Messages[0].Text != "source prompt" {
					t.Fatalf("full=%t source transcript changed: %+v", full, snapshot.Messages)
				}
				if len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != "old-turn" || snapshot.Turns[0].State != domain.TurnStateCompleted {
					t.Errorf("full=%t historical turns must contain only the source's completed turn: %+v", full, snapshot.Turns)
				}
			}
			live, err := svc.SnapshotPage(ctx, fresh.ID, 0, 10)
			if err != nil || len(live.Turns) != 1 || live.Turns[0].ID != "successor-turn" || live.Turns[0].State != state {
				t.Fatalf("successor live turn changed: turns=%+v err=%v", live.Turns, err)
			}
		})
	}
}
