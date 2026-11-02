package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
)

type attemptRetryHTTPControl struct {
	settingsControls
	calls int
	input agentsettings.RetryAttempt
}

func (c *attemptRetryHTTPControl) Retry(_ context.Context, _ agentsettings.Caller, _ domain.SessionID, _ string, in agentsettings.RetryAttempt) (domain.AgentSettingsReconciliation, error) {
	c.calls++
	c.input = in
	return domain.AgentSettingsReconciliation{ID: "retry-attempt", Phase: "blocked", RecoveryActions: []string{"wait_for_provider"}}, nil
}
func TestSettingsRetryRequiresVersionAndRejectsClaimedNativeEvidence(t *testing.T) {
	c := &attemptRetryHTTPControl{}
	router := chi.NewRouter()
	(&AgentSettingsController{Control: c}).Register(router)
	for _, test := range []struct {
		body   string
		status int
	}{
		{`{"attemptId":"old","expectedVersion":0,"expectedRevision":2}`, 202},
		{`{"attemptId":"old","expectedVersion":0}`, 400},
		{`{"attemptId":"old","expectedVersion":null,"expectedRevision":2}`, 400},
		{`{"attemptId":"old","expectedVersion":0,"expectedRevision":2,"providerEpoch":"forged"}`, 400},
	} {
		r := httptest.NewRequest(http.MethodPost, "/sessions/worker/agent-settings/operations/original/retry", strings.NewReader(test.body))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s: %d %s", test.body, w.Code, w.Body.String())
		}
	}
	if c.calls != 1 || c.input.AttemptID != "old" || *c.input.ExpectedRevision != 2 {
		t.Fatal("invalid retry reached runtime")
	}
}
