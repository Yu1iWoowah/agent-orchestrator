package controllers

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type settingsControls struct {
	caller agentsettings.Caller
	input  agentsettings.Change
}

func (s *settingsControls) Change(_ context.Context, caller agentsettings.Caller, _ domain.SessionID, in agentsettings.Change) (domain.AgentSettingsOperation, error) {
	s.caller = caller
	s.input = in
	if in.ExpectedRevision == nil {
		return domain.AgentSettingsOperation{}, apierr.Invalid("AGENT_SETTINGS_INVALID", "expectedRevision required", nil)
	}
	return domain.AgentSettingsOperation{ID: "op", Phase: domain.AgentSettingsPending}, nil
}
func (*settingsControls) Operation(context.Context, agentsettings.Caller, domain.SessionID, string) (domain.AgentSettingsOperation, error) {
	return domain.AgentSettingsOperation{ID: "op", Phase: domain.AgentSettingsApplied}, nil
}
func (*settingsControls) Cancel(context.Context, agentsettings.Caller, domain.SessionID, string, int64) (domain.AgentSettingsOperation, error) {
	return domain.AgentSettingsOperation{ID: "op", Phase: domain.AgentSettingsCancelled}, nil
}
func (*settingsControls) ReadForCaller(ctx context.Context, _ agentsettings.Caller, id domain.SessionID) (agentsettings.Snapshot, error) {
	return (settingsReader{}).Read(ctx, id)
}

func TestAgentSettingsMutationRoutesKeepPendingAndRequestIdentity(t *testing.T) {
	control := &settingsControls{}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	(&AgentSettingsController{Svc: settingsReader{}, Control: control}).Register(r)
	for _, tc := range []struct {
		method, path, body string
		status             int
		phase              string
	}{
		{"PATCH", "/sessions/worker/agent-settings", `{"expectedRevision":0,"settings":{"fastMode":"off"}}`, 202, "pending"},
		{"GET", "/sessions/worker/agent-settings/operations/op", "", 200, "applied"},
		{"POST", "/sessions/worker/agent-settings/operations/op/cancel", `{"expectedVersion":0}`, 200, "cancelled"},
		{"PATCH", "/sessions/worker/agent-settings", `{"settings":{"model":null}}`, 400, ""},
		{"PATCH", "/sessions/worker/agent-settings", `{"expectedRevision":0,"settings":{"account":"private"}}`, 400, ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Idempotency-Key", "key")
		req.Header.Set("X-Request-ID", "request")
		req.Header.Set("X-AO-Settings-Grant", "private-bearer")
		req.Header.Set("X-AO-Session-ID", "orch")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "private-bearer") || tc.phase != "" && !strings.Contains(w.Body.String(), `"phase":"`+tc.phase+`"`) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if tc.status == 400 && !strings.Contains(w.Body.String(), `"requestId":"request"`) {
			t.Fatalf("lost request id: %s", w.Body.String())
		}
	}
	if control.caller.SessionID != "orch" || control.caller.Grant != "private-bearer" || control.input.IdempotencyKey != "key" || control.input.RequestID != "request" {
		t.Fatalf("lost request identity: %+v", control.input)
	}
}
