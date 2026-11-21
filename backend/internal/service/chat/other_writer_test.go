package chat_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestImportedClaudeWriterBlocksNewDispatchAndKeepsSendReceipt(t *testing.T) {
	h, provider := hibernationHarnessForKind(t, domain.TurnStateCompleted, domain.KindOrchestrator, func(*chatsvc.Options) {}, func(session *domain.SessionRecord) {
		session.Harness = domain.HarnessClaudeCode
		session.Metadata.ImportSource = &domain.SessionImportSource{NativeID: newFakeConversation().ProviderConversationID(), Adopted: true}
	})
	ctx := t.Context()
	if saved, found, err := h.st.GetSession(ctx, testSession); err != nil || !found || saved.Metadata.ImportSource == nil {
		t.Fatalf("restore adopted import: found=%v err=%v", found, err)
	}
	h.svc.SetOtherWriterCheck(func(_ context.Context, record domain.SessionRecord, since time.Time) (bool, bool) {
		if record.ID != testSession || since.IsZero() {
			t.Errorf("unexpected writer scope: %s since %v", record.ID, since)
		}
		return true, false
	})

	before, err := h.svc.Snapshot(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	newMessage := ports.ChatUserMessage{Text: "new input", ClientMessageID: "blocked-write"}
	if _, err := h.svc.Send(ctx, testSession, newMessage); !errors.Is(err, chatsvc.ErrChatOpenElsewhere) {
		t.Fatalf("send = %v, want external writer conflict", err)
	}
	if _, err := h.svc.Steer(ctx, testSession, newMessage); !errors.Is(err, chatsvc.ErrChatOpenElsewhere) {
		t.Fatalf("steer = %v, want external writer conflict", err)
	}
	if _, err := h.svc.SteerOrSend(ctx, testSession, newMessage, false); !errors.Is(err, chatsvc.ErrChatOpenElsewhere) {
		t.Fatalf("steer-or-send = %v, want external writer conflict", err)
	}
	if _, err := h.svc.RelayChatTurnWithID(ctx, testSession, newMessage.Text, newMessage.ClientMessageID); !errors.Is(err, chatsvc.ErrChatOpenElsewhere) {
		t.Fatalf("relay = %v, want external writer conflict", err)
	}
	sent := ports.ChatUserMessage{Text: "do work", ClientMessageID: "hibernate-1"}
	if turn, err := h.svc.Send(ctx, testSession, sent); err != nil || turn.ID != "" {
		t.Fatalf("accepted send replay = %+v, %v", turn, err)
	}
	if result, err := h.svc.SteerOrSend(ctx, testSession, sent, true); err != nil || !result.Duplicate || result.Turn.ID != before.Turns[0].ID {
		t.Fatalf("recover-only replay = %+v, %v", result, err)
	}
	sent.Text = "changed"
	if _, err := h.svc.Send(ctx, testSession, sent); !errors.Is(err, domain.ErrClientMessageConflict) {
		t.Fatalf("changed accepted send = %v, want client message conflict", err)
	}
	after, err := h.svc.Snapshot(ctx, testSession)
	if err != nil || !reflect.DeepEqual(before, after) || len(provider.sentTexts()) != 1 || provider.calls.Load() != 0 {
		t.Fatalf("writer conflict changed delivery state: sends=%v stops=%d err=%v", provider.sentTexts(), provider.calls.Load(), err)
	}
}
