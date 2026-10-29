package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	fixturestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestReplacementLateReceiptSurvivesRecoveryCompletionAndWriterRotation(t *testing.T) {
	for _, phase := range []domain.ReplacementPhase{domain.ReplacementRecoveryRequired, domain.ReplacementCompleted} {
		t.Run(string(phase), func(t *testing.T) {
			f := newIntakeTransferFixture(t)
			ctx := context.Background()
			op, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now())
			if err != nil || !applied {
				t.Fatal(applied, err)
			}
			svc := lateIntakeChat(f.st, f.st)
			msg := ports.ChatUserMessage{ClientMessageID: "durable-late-client", Text: "accepted once", Origin: domain.MessageOriginHuman}
			accepted, err := svc.Send(ctx, f.op.SourceSessionID, msg)
			if err != nil {
				t.Fatal(err)
			}
			op, _, err = f.st.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			op.Phase = phase
			if phase == domain.ReplacementCompleted {
				fixturestore.SeedReplacementTestFacts(t, f.st, op)
			} else if saved, err := f.st.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			writer := f.writer
			writer.Metadata.BrowserCapabilityVerifier = "new-writer-capability"
			if err := f.st.UpdateSession(ctx, writer); err != nil {
				t.Fatal(err)
			}
			replayed, err := svc.Send(ctx, f.op.SourceSessionID, msg)
			if err != nil || replayed.ID != accepted.ID || replayed.ConversationID != accepted.ConversationID {
				t.Fatalf("committed late receipt lost after %s: turn=%s err=%v", phase, replayed.ID, err)
			}
			msg.Text = "different payload"
			if _, err := svc.Send(ctx, f.op.SourceSessionID, msg); !errors.Is(err, domain.ErrClientMessageConflict) {
				t.Fatal("replay failed to enforce its original payload", err)
			}
			msg.ClientMessageID = "new-input-without-writer-proof"
			if _, err := svc.Send(ctx, f.op.SourceSessionID, msg); !errors.Is(err, domain.ErrReplacementIntakeFenced) {
				t.Fatal("new input bypassed the current writer fence", err)
			}
			var count int
			if err := f.raw.QueryRow(`SELECT count(*) FROM conversation_messages WHERE client_message_id='durable-late-client'`).Scan(&count); err != nil || count != 1 {
				t.Fatal("late replay duplicated input", count, err)
			}
		})
	}
}

func TestReplacementCompletedExcerptUsesActivatedCurrentWriter(t *testing.T) {
	for _, fault := range []string{"current", "capability", "lease epoch", "lease frozen"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeTransferFixture(t)
			ctx := context.Background()
			op, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now())
			if err != nil || !applied {
				t.Fatal(applied, err)
			}
			op.Phase = domain.ReplacementCompleted
			fixturestore.SeedReplacementTestFacts(t, f.st, op)
			if fault == "capability" {
				writer := f.writer
				writer.Metadata.BrowserCapabilityVerifier = "rotated"
				if err := f.st.UpdateSession(ctx, writer); err != nil {
					t.Fatal(err)
				}
			} else if fault != "current" {
				state, err := f.st.GetProjectDocumentState(ctx, op.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "lease epoch" {
					state.Lease.Epoch++
				} else {
					state.Lease.Frozen = true
				}
				if saved, err := f.st.SaveProjectDocumentState(ctx, op.ProjectID, state); err != nil || !saved {
					t.Fatal(saved, err)
				}
			}
			message, _, err := f.st.ConversationMessageByClientID(ctx, f.target.ID, "pending-client")
			if err != nil {
				t.Fatal(err)
			}
			hash, err := domain.ReplacementIntakePayloadHash(domain.QueuedTurn{TurnID: message.TurnID, Text: message.Text, ClientMessageID: message.ClientMessageID, Origin: message.Origin, DeliveryContentJSON: message.DeliveryContentJSON})
			if err != nil {
				t.Fatal(err)
			}
			allowed, err := f.api.ReplacementExcerptSourceAllowed(ctx, f.writer.ID, f.writer.Metadata.ControllerGeneration, f.target.ID, message.TurnID, f.source.ID, hash)
			if err != nil || allowed != (fault == "current") {
				t.Fatalf("completed excerpt current writer ignored: fault=%s allowed=%t err=%v", fault, allowed, err)
			}
		})
	}
}

func TestReplacementReleasedProjectLateHumanProjectionAndAutomation(t *testing.T) {
	f := newIntakeTransferFixture(t)
	ctx := context.Background()
	if _, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); err != nil || !applied {
		t.Fatal(applied, err)
	}
	svc := lateIntakeChat(f.st, f.st)
	at := time.Now().Add(10 * time.Second).UTC()
	msg := ports.ChatUserMessage{Text: "human-authored project reply", ClientMessageID: "project-authored", Origin: domain.MessageOriginAutomation, AuthoredByUser: true, InteractionAt: at, Excerpts: []ports.ChatExcerptReference{{ConversationID: f.target.ID, MessageID: "pending-message", Revision: 1, Text: "A-Q28"}}}
	accepted, err := svc.Send(ctx, f.op.SourceSessionID, msg)
	if err != nil {
		t.Fatal(err)
	}
	writer, _, err := f.st.GetSession(ctx, f.writer.ID)
	if err != nil || writer.Metadata.LatestUserPrompt != msg.Text || !writer.Metadata.LatestUserPromptAt.Equal(at) {
		t.Errorf("released project input lost human projection: prompt=%q err=%v", writer.Metadata.LatestUserPrompt, err)
	}
	var receipt int
	if err := f.raw.QueryRow(`SELECT count(*) FROM replacement_human_intake_receipts WHERE message_id=(SELECT id FROM conversation_messages WHERE turn_id=?)`, accepted.ID).Scan(&receipt); err != nil || receipt != 1 {
		t.Errorf("released project lost authorship receipt: count=%d err=%v", receipt, err)
	}
	writer.Metadata.LatestUserPrompt = "later verified native prompt"
	writer.Metadata.ConversationCheckpointState = domain.ConversationCheckpointComplete
	writer.Metadata.ConversationCheckpointTurnID = "later-native-proof"
	if err := f.st.UpdateSession(ctx, writer); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send(ctx, f.op.SourceSessionID, ports.ChatUserMessage{Text: "ordinary automation", ClientMessageID: "automation-only", Origin: domain.MessageOriginAutomation}); err != nil {
		t.Fatal(err)
	}
	current, _, err := f.st.GetSession(ctx, f.writer.ID)
	if err != nil || current.Metadata.LatestUserPrompt != writer.Metadata.LatestUserPrompt || current.Metadata.ConversationCheckpointState != domain.ConversationCheckpointComplete || current.Metadata.ConversationCheckpointTurnID != "later-native-proof" {
		t.Fatal("ordinary automation rewrote human/native projection", current.Metadata.LatestUserPrompt, current.Metadata.ConversationCheckpointState, err)
	}
}
