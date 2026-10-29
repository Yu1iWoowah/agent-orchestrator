package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	fixturestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestReplacementQueueFenceDoesNotBlockWorkerConversation(t *testing.T) {
	for _, which := range []string{"source", "project", "worker"} {
		t.Run(which, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
			if err != nil {
				t.Fatal(err)
			}
			id, generation, scope := op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationScopeSession
			if which == "project" {
				scope = domain.ConversationScopeProject
			}
			if which == "worker" {
				seed := sampleRecord("replace")
				seed.Mode = domain.SessionModeChat
				seed.Metadata.ControllerGeneration = "worker-generation"
				worker, err := st.CreateSession(ctx, seed)
				if err != nil {
					t.Fatal(err)
				}
				id, generation = worker.ID, worker.Metadata.ControllerGeneration
			}
			now := time.Now().UTC()
			conv, err := st.CreateConversation(ctx, "queue", scope, op.ProjectID, id, now)
			if err != nil {
				t.Fatal(err)
			}
			payload := `[{"type":"image","mimeType":"image/png","data":"aW1hZ2U="}]`
			if fresh, err := st.AppendUserMessage(ctx, conv.ID, id, generation, domain.ConversationMessage{ID: "message", ClientMessageID: "client", Text: "canonical queue", Origin: domain.MessageOriginHuman, DeliveryContentJSON: payload}, "turn", now); err != nil || !fresh {
				t.Fatal(fresh, err)
			}
			next, err := st.NextQueuedTurn(ctx, conv.ID)
			if which == "worker" {
				if err != nil || next.TurnID != "turn" {
					t.Fatal("worker cannot drain", next, err)
				}
			} else if !errors.Is(err, domain.ErrNoQueuedTurn) {
				t.Fatalf("%s held turn became queue head: %+v err=%v", which, next, err)
			}
			listed, err := st.ListQueuedBatch(ctx, conv.ID, 0)
			if err != nil && (which == "worker" || !errors.Is(err, domain.ErrNoQueuedTurn)) {
				t.Fatal(err)
			}
			want := 0
			if which == "worker" {
				want = 1
			}
			if len(listed) != want {
				t.Fatal("batch reader escaped fence", which, listed)
			}
			claimed, err := st.ClaimQueuedBatch(ctx, conv.ID, true, 0, now)
			if which == "worker" {
				if err != nil || len(claimed) != 1 {
					t.Fatal("worker claim fenced", claimed, err)
				}
				return
			}
			if !errors.Is(err, domain.ErrNoQueuedTurn) || len(claimed) != 0 {
				t.Fatal("held queue claimed", claimed, err)
			}
			rows, err := st.LoadConversationSnapshot(ctx, conv.ID)
			if err != nil || len(rows.Messages) != 1 || len(rows.Turns) != 1 || rows.Messages[0].DeliveryContentJSON != payload || rows.Messages[0].ClientMessageID != "client" || rows.Turns[0].State != domain.TurnStateQueued || rows.Turns[0].ProviderTurnID != "" {
				t.Fatal("hold changed canonical receipt", rows, err)
			}
		})
	}
}

func TestReplacementWorkerReadyQueueAppendKeepsControllerFallback(t *testing.T) {
	for _, before := range []string{"provisioning", "hibernated"} {
		t.Run(before, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			seed := sampleRecord(string(op.ProjectID))
			seed.Mode = domain.SessionModeChat
			seed.Metadata.ControllerGeneration = "worker-generation"
			seed.Metadata.ProviderConversationID = "native-worker"
			seed.ProvisionState = domain.SessionProvisionProvisioning
			if before == "hibernated" {
				seed.ProvisionState = domain.SessionProvisionReady
				seed.HibernatedAt = &now
			}
			worker, err := st.CreateSession(ctx, seed)
			if err != nil {
				t.Fatal(err)
			}
			conv, err := st.CreateConversation(ctx, "worker-queue", domain.ConversationScopeSession, op.ProjectID, worker.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			// The caller read the old state; startup/wake commits before append.
			worker.ProvisionState, worker.HibernatedAt = domain.SessionProvisionReady, nil
			if err := st.UpdateSession(ctx, worker); err != nil {
				t.Fatal(err)
			}
			if ready, err := st.SetSessionProvisionState(ctx, worker.ID, domain.SessionProvisionReady, "", now); err != nil || !ready {
				t.Fatalf("finish worker startup: ready=%t err=%v", ready, err)
			}
			msg := domain.ConversationMessage{ID: "worker-message", ClientMessageID: "worker-client", Text: "source can still send to its worker", Origin: domain.MessageOriginAutomation, SenderSessionID: string(op.SourceSessionID), SenderProjectID: string(op.ProjectID)}
			created, err := st.AppendUserMessage(ctx, conv.ID, worker.ID, "", msg, "worker-turn", now)
			if created || !errors.Is(err, domain.ErrSessionNotProvisioning) {
				t.Fatalf("stale worker append must fall back to its controller: created=%t err=%v", created, err)
			}
			snapshot, err := st.LoadConversationSnapshot(ctx, conv.ID)
			if err != nil || len(snapshot.Messages) != 0 || len(snapshot.Turns) != 0 {
				t.Fatalf("stale queue append left a partial receipt: %+v err=%v", snapshot, err)
			}
			if created, err := st.AppendUserMessage(ctx, conv.ID, worker.ID, worker.Metadata.ControllerGeneration, msg, "worker-turn", now); err != nil || !created {
				t.Fatalf("live worker controller could not accept input: created=%t err=%v", created, err)
			}
			batch, err := st.ClaimQueuedBatch(ctx, conv.ID, true, 0, now)
			if err != nil || len(batch) != 1 || batch[0].TurnID != "worker-turn" || batch[0].ClientMessageID != msg.ClientMessageID || batch[0].Text != msg.Text {
				t.Fatalf("worker fallback changed or blocked canonical input: %+v err=%v", batch, err)
			}
		})
	}
}

func TestReplacementCancelledPromotingControlDoesNotBlockLaterInput(t *testing.T) {
	for _, lateRefusal := range []bool{false, true} {
		t.Run(map[bool]string{false: "still reserved", true: "late provider refusal"}[lateRefusal], func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			text := "prepare exact handoff"
			sum := sha256.Sum256([]byte(text))
			op.Phase = domain.ReplacementPreparingHandoff
			op.Preparation = &domain.ReplacementPreparation{AttemptID: "33333333-3333-4333-8333-333333333333", Nonce: "nonce", LeaseEpoch: 7, ControlClientID: "replacement/" + op.ID + "/prepare", ControlTurnID: "control-turn", PromptHash: hex.EncodeToString(sum[:]), PromptIntentAt: &now, StartedAt: now, DeadlineAt: op.DeadlineAt}
			op = fixturestore.SeedReplacementTestFacts(t, st, op)
			conv, err := st.CreateConversation(ctx, "queue", domain.ConversationScopeSession, op.ProjectID, op.SourceSessionID, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AppendUserMessage(ctx, conv.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "control-message", ClientMessageID: op.Preparation.ControlClientID, Text: text, Origin: domain.MessageOriginAutomation}, "control-turn", now); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ReserveQueuedTurnForPromotion(ctx, conv.ID, "control-turn", now); err != nil {
				t.Fatal(err)
			}
			op, applied, err := st.ResetReplacementBeforeStop(ctx, op.ID, op.Revision, true, now, now.Add(time.Hour))
			if err != nil || !applied || op.Phase != domain.ReplacementCancelled {
				t.Fatal("cancel interrupted promotion or retained op", applied, op.Phase, err)
			}
			if lateRefusal {
				if err := st.ReleaseQueuedTurnPromotion(ctx, conv.ID, "control-turn"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.AppendUserMessage(ctx, conv.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "later-message", ClientMessageID: "later-client", Text: "continue normal work", Origin: domain.MessageOriginHuman}, "later-turn", now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			next, err := st.NextQueuedTurn(ctx, conv.ID)
			if err != nil || next.TurnID != "later-turn" {
				t.Fatalf("archived control blocked next input: next=%+v err=%v", next, err)
			}
			batch, err := st.ClaimQueuedBatch(ctx, conv.ID, true, 0, now)
			if err != nil || len(batch) != 1 || batch[0].TurnID != "later-turn" {
				t.Fatalf("archived control blocked batch: %+v err=%v", batch, err)
			}
			control, err := st.TurnByID(ctx, "control-turn")
			if err != nil || control.State != domain.TurnStateQueued || control.ProviderTurnID != "" {
				t.Fatal("drain changed archived control evidence", control, err)
			}
		})
	}
}

func TestReplacementDirectDispatchKeepsHeldReceiptAtomic(t *testing.T) {
	for _, action := range []string{"mark", "bind", "reserve"} {
		t.Run(action, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			conv, err := st.CreateConversation(ctx, "queue", domain.ConversationScopeSession, op.ProjectID, op.SourceSessionID, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AppendUserMessage(ctx, conv.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "message", ClientMessageID: "client", Text: "held", Origin: domain.MessageOriginHuman}, "turn", now); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "mark":
				err = st.MarkTurnDispatching(ctx, "turn")
			case "bind":
				err = st.BindTurnToProvider(ctx, "turn", "never-accepted-provider", now)
			case "reserve":
				_, err = st.ReserveQueuedTurnForPromotion(ctx, conv.ID, "turn", now)
			}
			turn, readErr := st.TurnByID(ctx, "turn")
			if readErr != nil || turn.ProviderTurnID != "" || turn.StartedAt != nil || turn.State != domain.TurnStateQueued {
				t.Fatalf("rejected dispatch minted delivery evidence: turn=%+v err=%v", turn, readErr)
			}
			if !errors.Is(err, domain.ErrReplacementIntakeFenced) {
				t.Fatalf("dispatch did not expose named fence: %v", err)
			}
		})
	}
}
