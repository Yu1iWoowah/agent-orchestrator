package chat_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestHOwnerInboxUnboundSourceDoesNotBlockChatPiggybackSend(t *testing.T) {
	h := newHarness(t)
	worker, err := h.st.CreateSession(t.Context(), domain.SessionRecord{ProjectID: testProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: h.now(), UpdatedAt: h.now()})
	if err != nil {
		t.Fatal(err)
	}
	core := store.NewOwnerInboxCore(h.st)
	accepted, err := core.AcceptReport(t.Context(), domain.InboxReportIntent{Key: "h-source", Report: domain.ReportRecord{ID: "h-source", SessionID: worker.ID, ProjectID: testProject, State: domain.ReportCheckpoint, Note: "unbound H source", CreatedAt: h.now(), AvailableAt: h.now(), DeliveryState: domain.ReportPending, RepeatCount: 1}})
	if err != nil {
		t.Fatal(err)
	}
	before, found, err := core.GetByActionKey(t.Context(), testProject, accepted.Key)
	if err != nil || !found {
		t.Fatalf("H source: found=%t err=%v", found, err)
	}
	if _, err := h.st.CreateReport(t.Context(), domain.ReportRecord{ID: "legacy-report", SessionID: worker.ID, ProjectID: testProject, State: domain.ReportCheckpoint, Note: "ordinary worker result", CreatedAt: h.now(), AvailableAt: h.now().Add(time.Hour), DeliveryState: domain.ReportPending, RepeatCount: 1}); err != nil {
		t.Fatal(err)
	}
	h.svc.SetReportCoordinator(reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, Now: h.now, NewToken: func() string { return "chat-piggyback" }}))
	if _, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "continue my work", ClientMessageID: "human-with-reports", Origin: domain.MessageOriginHuman}); err != nil {
		t.Fatalf("unbound H source blocked Send: %v", err)
	}
	message, found, err := h.st.ConversationMessageByClientID(t.Context(), h.ctrl.ConversationID(), "human-with-reports")
	if err != nil || !found || message.Origin != domain.MessageOriginHuman || !strings.Contains(message.Text, "ordinary worker result") || strings.Contains(message.Text, "unbound H source") {
		t.Fatalf("piggyback message: %+v found=%t err=%v", message, found, err)
	}
	if _, found, err := h.st.GetReport(t.Context(), "h-source"); err != nil || found {
		t.Fatalf("H source leaked into legacy reports: found=%t err=%v", found, err)
	}
	legacy, found, err := h.st.GetReport(t.Context(), "legacy-report")
	if err != nil || !found || legacy.DeliveryState != domain.ReportAcknowledged {
		t.Fatalf("ordinary report not ACKed: %+v found=%t err=%v", legacy, found, err)
	}
	after, found, err := core.GetByActionKey(t.Context(), testProject, accepted.Key)
	if err != nil || !found || !reflect.DeepEqual(before, after) {
		t.Fatalf("H source changed: before=%+v after=%+v found=%t err=%v", before, after, found, err)
	}
}
