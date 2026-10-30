package controllers_test

import (
	"context"
	"encoding/json"
	"errors"
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
	err      error
}

func (s noticeReadAPI) WorkerNotificationReceipts(context.Context, string) ([]domain.ReplacementNoticeReceipt, error) {
	return s.receipts, s.err
}

func TestReplacementHTTPKeepsCommittedResultWhenNoticeReceiptsUnavailable(t *testing.T) {
	for _, failure := range []string{"read error", "invalid projection"} {
		for _, action := range []string{"accept", "get", "retry", "cancel"} {
			t.Run(failure+"/"+action, func(t *testing.T) {
				op := replacementAPIState()
				base := &replacementAPI{op: op}
				svc := noticeReadAPI{replacementAPI: base}
				if failure == "read error" {
					svc.err = errors.New("private-receipt-capability read failed")
				}
				router := chi.NewRouter()
				(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
				method, path, body := http.MethodPost, "/orchestrator/replacements/"+op.ID+"/"+action, `{}`
				want := http.StatusAccepted
				switch action {
				case "get":
					method, path, want = http.MethodGet, "/orchestrator/replacements/"+op.ID, http.StatusOK
				case "accept":
					path, body = "/projects/demo/orchestrator/replacements", `{"sourceSessionId":"source","operationKey":"`+op.OperationKey+`"}`
				}
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
				if w.Code != want {
					t.Fatalf("durable %s result was replaced by receipt failure: status=%d body=%s", action, w.Code, w.Body.String())
				}
				var response struct {
					Operation struct {
						ID          string            `json:"id"`
						Unavailable bool              `json:"notificationReceiptsUnavailable"`
						Receipts    []json.RawMessage `json:"notificationReceipts"`
					} `json:"operation"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Operation.ID != op.ID || !response.Operation.Unavailable || len(response.Operation.Receipts) != 0 {
					t.Fatalf("missing explicit unavailable receipt view: body=%s err=%v", w.Body.String(), err)
				}
				if len(base.calls) != 1 || strings.Contains(w.Body.String(), "private-receipt-capability") {
					t.Fatal("receipt fallback repeated mutation or leaked private error", base.calls, w.Body.String())
				}
			})
		}
	}
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
