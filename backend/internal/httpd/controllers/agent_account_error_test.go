package controllers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAccountInUseWithoutOperationDoesNotClaimScheduledCleanup(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/accounts/saved", nil)
	writeAgentAccountError(w, r, ports.ErrAgentAccountInUse)
	if w.Code != 409 || strings.Contains(w.Body.String(), "cleanup is scheduled") || !strings.Contains(w.Body.String(), "has not been changed") {
		t.Fatalf("refused mutation reported as scheduled: %d %s", w.Code, w.Body.String())
	}
}
