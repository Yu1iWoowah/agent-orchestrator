package controllers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

type noticeReadAPI struct {
	*replacementAPI
	receipts []domain.ReplacementNoticeReceipt
}

func (s noticeReadAPI) WorkerNotificationReceipts(context.Context, string) ([]domain.ReplacementNoticeReceipt, error) {
	return s.receipts, nil
}
func TestReplacementHTTPProjectsDurableNoticeEvidenceWithoutPrivateClaims(t *testing.T) {
	op := replacementAPIState()
	op.Notifications = nil
	notices := []domain.ReplacementNoticeReceipt{
		{SessionID: "chat-worker", State: domain.ReplacementNoticeAccepted, Mode: domain.SessionModeChat, EvidenceKind: "chat_queue", ConversationID: "worker-conversation", MessageID: "canonical-message", TurnID: "canonical-turn", SettledAt: &op.UpdatedAt},
		{SessionID: "tui-worker", State: domain.ReplacementNoticeAccepted, Mode: domain.SessionModeTUI, EvidenceKind: "tui_checkpoint", TurnID: "handoff-notice-attempt:private-nonce", SettledAt: &op.UpdatedAt},
		{SessionID: "sending-worker", State: domain.ReplacementNoticeUncertain, Mode: domain.SessionModeTUI, ErrorCode: "DELIVERY_UNCONFIRMED"},
		{SessionID: "terminated-worker", State: domain.ReplacementNoticeSkipped, EvidenceKind: "terminated", ErrorCode: "TARGET_TERMINATED", SettledAt: &op.UpdatedAt},
	}
	for i := range notices {
		n := &notices[i]
		n.OperationID, n.ContactID, n.CreatedAt, n.AttemptID = op.ID, "handoff-notice:"+string(n.SessionID), op.CreatedAt, "private-nonce"
		n.Owner = domain.SessionControllerOwner{ControllerGeneration: "private-owner", AgentSessionID: "private-native"}
		op.Notifications = append(op.Notifications, domain.ReplacementWorkerNotification{SessionID: n.SessionID, ContactID: n.ContactID, State: "pending", CreatedAt: n.CreatedAt})
	}
	router := chi.NewRouter()
	(&controllers.OrchestratorReplacementController{Svc: noticeReadAPI{replacementAPI: &replacementAPI{op: op}, receipts: notices}}).Register(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/orchestrator/replacements/"+op.ID, nil))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, required := range []string{`"state":"accepted"`, `"state":"uncertain"`, `"state":"skipped"`, `"evidenceKind":"chat_queue"`, `"evidenceKind":"tui_checkpoint"`, `"messageId":"canonical-message"`, `"turnId":"canonical-turn"`, `"errorCode":"TARGET_TERMINATED"`, `"settledAt"`} {
		if !strings.Contains(body, required) {
			t.Errorf("missing durable notice fact %s: %s", required, body)
		}
	}
	for _, private := range []string{"private-nonce", "private-owner", "private-native", "handoff-notice-attempt:", `"state":"sending"`} {
		if strings.Contains(body, private) {
			t.Errorf("leaked private claim %s", private)
		}
	}
}
