package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type fakePlanConversationService struct {
	*fakeConversationService
	plan    chatsvc.PlanSnapshot
	history chatsvc.PlanHistory
	err     error
	session domain.SessionID
	before  int64
	limit   int64
}

func (f *fakePlanConversationService) Plan(_ context.Context, id domain.SessionID) (chatsvc.PlanSnapshot, error) {
	f.session = id
	return f.plan, f.err
}

func (f *fakePlanConversationService) PlanHistory(_ context.Context, id domain.SessionID, before, limit int64) (chatsvc.PlanHistory, error) {
	f.session, f.before, f.limit = id, before, limit
	return f.history, f.err
}

func planTestServer(t *testing.T, svc *fakePlanConversationService) *httptest.Server {
	t.Helper()
	svc.fakeConversationService = &fakeConversationService{}
	s := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		httpd.APIDeps{Sessions: newFakeSessionService(), Conversations: svc}, httpd.ControlDeps{}))
	t.Cleanup(s.Close)
	return s
}

func TestSessionPlanWireStates(t *testing.T) {
	for _, state := range []string{"absent", "cleared", "present"} {
		t.Run(state, func(t *testing.T) {
			svc := &fakePlanConversationService{plan: chatsvc.PlanSnapshot{ConversationID: "conv-1", ActiveBranchID: "branch-1"}}
			if state != "absent" {
				svc.plan.Version = &domain.ConversationPlanVersion{ConversationID: "conv-1", BranchID: "branch-1", TurnID: "turn-1",
					SourceActivityID: "activity-1", Sequence: 42, Revision: 3, UpdatedAt: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
					Plan: domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{}}}
				if state == "present" {
					svc.plan.Version.Plan.Steps = []domain.ConversationPlanStep{{Text: "one", Status: domain.PlanStepInProgress}}
				}
			}
			server := planTestServer(t, svc)
			response, err := http.Get(server.URL + "/api/v1/sessions/ao-1/plan")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			var wire struct {
				State   string `json:"state"`
				Version *struct {
					SourceActivityID string `json:"sourceActivityId"`
					Sequence         int64  `json:"sequence"`
					Plan             struct {
						SpansTurns bool `json:"spansTurns"`
						Steps      []struct {
							Text string `json:"text"`
						} `json:"steps"`
					} `json:"plan"`
				} `json:"version"`
			}
			if err := json.NewDecoder(response.Body).Decode(&wire); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || wire.State != state || svc.session != "ao-1" {
				t.Fatalf("status=%d state=%q session=%q", response.StatusCode, wire.State, svc.session)
			}
			if state == "absent" && wire.Version != nil {
				t.Fatal("absent state invented a plan")
			}
			if state != "absent" && (wire.Version == nil || wire.Version.Sequence != 42 || wire.Version.SourceActivityID != "activity-1" || !wire.Version.Plan.SpansTurns) {
				t.Fatalf("plan provenance lost: %+v", wire.Version)
			}
			if state == "present" && (len(wire.Version.Plan.Steps) != 1 || wire.Version.Plan.Steps[0].Text != "one") {
				t.Fatalf("steps = %+v", wire.Version.Plan.Steps)
			}
			if state == "cleared" && wire.Version.Plan.Steps == nil {
				t.Fatal("clear must expose an empty array rather than null")
			}
		})
	}
}

func TestSessionPlanHistoryCursorAndValidation(t *testing.T) {
	svc := &fakePlanConversationService{history: chatsvc.PlanHistory{ConversationID: "conv-1", ActiveBranchID: "branch-1",
		Versions: []domain.ConversationPlanVersion{}, HasMoreBefore: true, NextBeforeSequence: 10}}
	server := planTestServer(t, svc)
	response, err := http.Get(server.URL + "/api/v1/sessions/ao-1/plan/history?beforeSequence=17&limit=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || svc.before != 17 || svc.limit != 1 || !strings.Contains(string(body), `"nextBeforeSequence":10`) || !strings.Contains(string(body), `"versions":[]`) {
		t.Fatalf("history status=%d before=%d limit=%d body=%s", response.StatusCode, svc.before, svc.limit, body)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=-1", "limit=bad", "beforeSequence=0", "beforeSequence=-1", "beforeSequence=bad"} {
		t.Run(query, func(t *testing.T) {
			r, err := http.Get(server.URL + "/api/v1/sessions/ao-1/plan/history?" + query)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Body.Close() }()
			if r.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid page status=%d", r.StatusCode)
			}
		})
	}
}

func TestSessionPlanPreservesConversationErrorEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"missing", ports.ErrSessionNotFound, http.StatusNotFound, "SESSION_NOT_FOUND"},
		{"terminal", chatsvc.ErrNotChatMode, http.StatusConflict, "SESSION_MODE_MISMATCH"},
		{"switching", chatsvc.ErrControllerHandoff, http.StatusConflict, "CHAT_INTERFACE_TRANSITION"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakePlanConversationService{err: tc.err}
			server := planTestServer(t, svc)
			r, err := http.Get(server.URL + "/api/v1/sessions/ao-1/plan")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Body.Close() }()
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				RequestID string `json:"requestId"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatal(err)
			}
			if r.StatusCode != tc.status || !strings.Contains(string(body), tc.code) || envelope.RequestID == "" {
				t.Fatalf("status=%d requestId=%q body=%s", r.StatusCode, envelope.RequestID, body)
			}
		})
	}
}
