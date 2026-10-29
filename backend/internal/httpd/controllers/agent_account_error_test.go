package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAccountInUseExplainsScheduledCleanup(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/accounts/saved", nil)
	writeAgentAccountError(w, r, ports.ErrAgentAccountInUse)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "scheduled") || !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("scheduled operation reported as rejection: %d %s", w.Code, w.Body.String())
	}
}
