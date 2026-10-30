package chat_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type prevalidatedWorkerNoticeStore struct{ *sqlite.Store }

// Transport unit boundary: production's sealed SQLite claim is exercised by
// coordinator/store regressions. Here the injected claim isolates queue policy.
func (s prevalidatedWorkerNoticeStore) ValidateReplacementWorkerNotice(ctx context.Context, id string, target domain.SessionID, attempt string, revision int64) (domain.OrchestratorReplacement, domain.SessionRecord, domain.ReplacementNoticeReceipt, error) {
	rec, _, err := s.GetSession(ctx, target)
	if err != nil {
		return domain.OrchestratorReplacement{}, rec, domain.ReplacementNoticeReceipt{}, err
	}
	c, err := s.ConversationForSession(ctx, target)
	op := domain.OrchestratorReplacement{ID: id, ProjectID: rec.ProjectID, SourceSessionID: "old", SuccessorSessionID: "new"}
	return op, rec, domain.ReplacementNoticeReceipt{Mode: domain.SessionModeChat, ConversationID: c.ID, Owner: rec.ControllerOwner(), ContactID: "handoff-notice:" + id + ":" + string(target)}, err
}
func TestReplacementWorkerChatFYIQueuesWithoutInterruptAndDeduplicates(t *testing.T) {
	provider := newInterruptRecorder()
	h := newHarnessWithConversationAndStore(t, provider, func(st *sqlite.Store) chatsvc.Store { return prevalidatedWorkerNoticeStore{st} })
	ctx := context.Background()
	rec, _, err := h.st.GetSession(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	rec.Metadata.ControllerGeneration, rec.Metadata.ProviderConversationID = h.ctrl.Generation(), h.ctrl.ProviderConversationID()
	if err := h.st.UpdateSession(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "worker current task", ClientMessageID: "current-work"}); err != nil {
		t.Fatal(err)
	}
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 && len(provider.sentTexts()) == 1 })
	api, ok := any(h.svc).(interface {
		DeliverReplacementChatWorkerFYI(context.Context, string, domain.SessionID, string, int64) error
	})
	if !ok {
		t.Fatal("queue-only worker FYI boundary unavailable")
	}
	for range 3 {
		if err := api.DeliverReplacementChatWorkerFYI(ctx, "operation", testSession, "private-attempt", 1); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := h.svc.Snapshot(ctx, testSession)
	if err != nil || len(snapshot.Turns) != 2 || len(snapshot.Messages) != 2 {
		t.Fatal("lost canonical receipt", snapshot, err)
	}
	var queued int
	for _, turn := range snapshot.Turns {
		if turn.State == domain.TurnStateQueued {
			queued++
		}
	}
	if queued != 1 || snapshot.Messages[1].Origin != domain.MessageOriginAutomation || snapshot.Messages[1].ClientMessageID != "handoff-notice:operation:"+string(testSession) {
		t.Fatal("FYI was not one queued automation", snapshot)
	}
	if texts := provider.sentTexts(); len(texts) != 1 || texts[0] != "worker current task" {
		t.Fatal("FYI dispatched while worker busy", texts)
	}
	provider.activeMu.Lock()
	interrupts := len(provider.attempts)
	provider.activeMu.Unlock()
	if interrupts != 0 || h.hostStops.Load() != 0 {
		t.Fatal("FYI interrupted/stopped worker", interrupts, h.hostStops.Load())
	}
}
