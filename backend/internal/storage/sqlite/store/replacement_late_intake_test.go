package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func lateIntakeChat(st chatsvc.Store, sessions *sqlite.Store) *chatsvc.Service {
	n := 0
	return chatsvc.New(chatsvc.Options{Store: st, Sessions: sessions, NewID: func() string { n++; return fmt.Sprintf("late-intake-%d", n) }})
}

func TestReplacementRetiredSourceKeepsIntakeBeforeSuccessorSeed(t *testing.T) {
	f := newCanonicalIntakeFixture(t, false, false)
	ctx := context.Background()
	if f.op.SuccessorSessionID != "" {
		t.Fatal("fixture already activated a writer")
	}
	svc := lateIntakeChat(f.st, f.st)
	message := ports.ChatUserMessage{Text: "Q30 remains unanswered", ClientMessageID: "late-source-client", Origin: domain.MessageOriginHuman, Content: []ports.ChatContent{{Type: "image", Data: "aW1hZ2U=", MIMEType: "image/png"}}}
	turn, err := svc.Send(ctx, f.op.SourceSessionID, message)
	if err != nil || turn.ID == "" || turn.State != domain.TurnStateQueued || turn.HandledBySessionID != f.op.SourceSessionID {
		t.Fatal("late source input lost", turn, err)
	}
	replay, err := svc.QueueReplacementIntake(ctx, f.op.SourceSessionID, message)
	if err != nil || replay.ID != turn.ID {
		t.Fatal("late acceptance duplicated", replay, err)
	}
	row, found, err := f.st.ConversationMessageByClientID(ctx, f.source.ID, message.ClientMessageID)
	if err != nil || !found || !strings.Contains(row.DeliveryContentJSON, "image/png") {
		t.Fatal("payload missing", row, found, err)
	}
	if _, err := f.st.NextQueuedTurn(ctx, turn.ConversationID); !errors.Is(err, domain.ErrNoQueuedTurn) {
		t.Fatal("retired source can dispatch", err)
	}
	if svc.HasLiveChatController(f.op.SourceSessionID) {
		t.Fatal("late input started a controller")
	}
}

func TestReplacementLateIntakeRoutesOldSourceAndRecoversAcrossRelease(t *testing.T) {
	f := newIntakeTransferFixture(t)
	ctx := context.Background()
	svc := lateIntakeChat(f.st, f.st)
	message := ports.ChatUserMessage{Text: "A-Q30: A", ClientMessageID: "late-stable-client", Origin: domain.MessageOriginHuman, Content: []ports.ChatContent{{Type: "resource", URI: "artifact://retained-reference", MIMEType: "text/plain"}}}
	before, err := svc.QueueReplacementIntake(ctx, f.op.SourceSessionID, message)
	if err != nil {
		t.Fatal(err)
	}
	original, found, err := f.st.ConversationMessageByClientID(ctx, f.source.ID, message.ClientMessageID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if _, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); err != nil || !applied {
		t.Fatal(applied, err)
	}
	after, err := svc.QueueReplacementIntake(ctx, f.op.SourceSessionID, message)
	if err != nil || after.ID != before.ID || after.ConversationID != f.target.ID || after.HandledBySessionID != f.writer.ID {
		t.Fatal("release lost original receipt", before, after, err)
	}
	retained, found, err := f.st.ConversationMessageByClientID(ctx, f.target.ID, message.ClientMessageID)
	if err != nil || !found || retained.ID != original.ID || retained.DeliveryContentJSON != original.DeliveryContentJSON || retained.ClientPayloadHash != original.ClientPayloadHash {
		t.Fatal("release rewrote accepted bytes", retained, found, err)
	}
	fresh := message
	fresh.ClientMessageID = "late-after-release"
	fresh.Text = "Q31 still open"
	direct, err := svc.Send(ctx, f.op.SourceSessionID, fresh)
	if err != nil || direct.ConversationID != f.target.ID || direct.HandledBySessionID != f.writer.ID {
		t.Fatal("late route stranded at old source", direct, err)
	}
	replay, err := svc.Send(ctx, f.op.SourceSessionID, fresh)
	if err != nil || replay.ID != direct.ID {
		t.Fatal("late route duplicated", replay, err)
	}
	changed := fresh
	changed.Text = "different request"
	if _, err := svc.Send(ctx, f.op.SourceSessionID, changed); !errors.Is(err, domain.ErrClientMessageConflict) {
		t.Fatal("client identity overwritten", err)
	}
	if svc.HasLiveChatController(f.op.SourceSessionID) || svc.HasLiveChatController(f.writer.ID) {
		t.Fatal("late receipt woke a controller")
	}
}

type lostLateReply struct {
	*sqlite.Store
	lost bool
	turn string
}

func (s *lostLateReply) AppendRetiredReplacementIntake(ctx context.Context, id domain.SessionID, original string, message domain.ConversationMessage, turn string, at time.Time) (domain.ConversationTurn, error) {
	result, err := s.Store.AppendRetiredReplacementIntake(ctx, id, original, message, turn, at)
	if err == nil && !s.lost {
		s.lost = true
		s.turn = result.ID
		return domain.ConversationTurn{}, errors.New("reply lost after canonical commit")
	}
	return result, err
}

func TestReplacementLateIntakeLostReplyRecoversOneCanonicalTurn(t *testing.T) {
	f := newIntakeTransferFixture(t)
	ctx := context.Background()
	if _, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); err != nil || !applied {
		t.Fatal(applied, err)
	}
	wrapped := &lostLateReply{Store: f.st}
	svc := lateIntakeChat(wrapped, f.st)
	message := ports.ChatUserMessage{Text: "recover this accepted input", ClientMessageID: "late-lost-reply", Origin: domain.MessageOriginHuman}
	if _, err := svc.Send(ctx, f.op.SourceSessionID, message); err == nil {
		t.Fatal("lost response was not simulated")
	}
	turn, err := svc.Send(ctx, f.op.SourceSessionID, message)
	if err != nil || turn.ID != wrapped.turn {
		t.Fatal("lost response created a new turn", turn, err)
	}
	var count int
	if err := f.raw.QueryRow(`SELECT count(*) FROM conversation_messages WHERE client_message_id=?`, message.ClientMessageID).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate canonical input", count, err)
	}
}

func TestReplacementLateIntakeRejectsOwnerRotationAndRollsBackTransferFailure(t *testing.T) {
	for _, fault := range []string{"source capability", "target capability", "ENOSPC"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeTransferFixture(t)
			ctx := context.Background()
			if _, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); err != nil || !applied {
				t.Fatal(applied, err)
			}
			if fault == "ENOSPC" {
				if _, err := f.raw.Exec(`CREATE TRIGGER refuse_late_move BEFORE UPDATE OF conversation_id ON conversation_messages WHEN NEW.client_message_id='rejected-late' BEGIN SELECT RAISE(ABORT,'late ENOSPC'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				id := f.writer.ID
				if fault == "source capability" {
					id = f.op.SourceSessionID
				}
				row, _, err := f.st.GetSession(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				row.Metadata.BrowserCapabilityVerifier = "rotated"
				if err := f.st.UpdateSession(ctx, row); err != nil {
					t.Fatal(err)
				}
			}
			svc := lateIntakeChat(f.st, f.st)
			if _, err := svc.Send(ctx, f.op.SourceSessionID, ports.ChatUserMessage{Text: "retain or reject atomically", ClientMessageID: "rejected-late", Origin: domain.MessageOriginHuman}); err == nil {
				t.Fatal("unverified late input accepted", fault)
			}
			var count int
			if err := f.raw.QueryRow(`SELECT count(*) FROM conversation_messages WHERE client_message_id='rejected-late'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial late message", count, err)
			}
		})
	}
}

func TestReplacementLateExcerptPreservesReferenceAndRejectsWorkerHistory(t *testing.T) {
	f := newIntakeTransferFixture(t)
	ctx := context.Background()
	svc := lateIntakeChat(f.st, f.st)
	reference := ports.ChatExcerptReference{ConversationID: f.source.ID, MessageID: "history-answer", Revision: 1, Text: "selected answer"}
	message := ports.ChatUserMessage{Text: "keep original context", ClientMessageID: "late-excerpt", Origin: domain.MessageOriginHuman, Excerpts: []ports.ChatExcerptReference{reference}}
	turn, err := svc.QueueReplacementIntake(ctx, f.op.SourceSessionID, message)
	if err != nil {
		t.Fatal(err)
	}
	if _, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); err != nil || !applied {
		t.Fatal(applied, err)
	}
	row, _, err := f.st.ConversationMessageByClientID(ctx, f.target.ID, message.ClientMessageID)
	if err != nil || row.TurnID != turn.ID || !strings.Contains(row.DeliveryContentJSON, reference.ConversationID) || !strings.Contains(row.DeliveryContentJSON, reference.MessageID) {
		t.Fatal("late excerpt reference changed", row, err)
	}
	worker, err := f.st.CreateConversation(ctx, "worker-private-history", domain.ConversationScopeSession, f.op.ProjectID, f.worker.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	message.ClientMessageID = "worker-private-excerpt"
	message.Excerpts[0].ConversationID = worker.ID
	if _, err := svc.QueueReplacementIntake(ctx, f.op.SourceSessionID, message); err == nil {
		t.Fatal("old source accepted worker-private history")
	}
	var count int
	if err := f.raw.QueryRow(`SELECT count(*) FROM conversation_messages WHERE client_message_id=?`, message.ClientMessageID).Scan(&count); err != nil || count != 0 {
		t.Fatal("forbidden reference persisted", count, err)
	}
}

func TestReplacementLateAuthoredAutomationProjectionAndReplayStayUnconfirmed(t *testing.T) {
	f := newIntakeTransferFixture(t)
	ctx := context.Background()
	svc := lateIntakeChat(f.st, f.st)
	if _, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); err != nil || !applied {
		t.Fatal(applied, err)
	}
	at := time.Now().Add(4 * time.Second).UTC()
	message := ports.ChatUserMessage{Text: "human-edited automation", ClientMessageID: "authored-late", Origin: domain.MessageOriginAutomation, AuthoredByUser: true, InteractionAt: at, SenderSessionID: string(f.worker.ID)}
	accepted, err := svc.Send(ctx, f.op.SourceSessionID, message)
	if err != nil || accepted.State != domain.TurnStateQueued {
		t.Fatal(accepted, err)
	}
	writer, _, err := f.st.GetSession(ctx, f.writer.ID)
	if err != nil || writer.Metadata.LatestUserPrompt != message.Text || !writer.Metadata.LatestUserPromptAt.Equal(at) || writer.Metadata.ConversationCheckpointState != domain.ConversationCheckpointLegacy {
		t.Fatal("late authorship/time projection was lost", writer.Metadata, err)
	}
	writer.Metadata.ConversationCheckpointState = domain.ConversationCheckpointComplete
	writer.Metadata.ConversationCheckpointTurnID = "later-native-proof"
	if err := f.st.UpdateSession(ctx, writer); err != nil {
		t.Fatal(err)
	}
	again, err := svc.Send(ctx, f.op.SourceSessionID, message)
	if err != nil || again.ID != accepted.ID {
		t.Fatal(again, err)
	}
	current, _, err := f.st.GetSession(ctx, f.writer.ID)
	if err != nil || current.Metadata.ConversationCheckpointTurnID != "later-native-proof" {
		t.Fatal("late replay rewrote native proof", current.Metadata, err)
	}
}
