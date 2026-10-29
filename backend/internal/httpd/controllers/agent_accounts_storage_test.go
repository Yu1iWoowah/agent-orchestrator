package controllers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAccountStorageErrorsAreCategorizedWithoutPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"occupied reserved home", ports.ErrAgentAccountUnavailableHomeOccupied, http.StatusConflict, "AGENT_ACCOUNT_UNAVAILABLE_HOME_OCCUPIED"},
		{"unsafe", ports.ErrAgentAccountStorageUnsafe, http.StatusConflict, "AGENT_ACCOUNT_STORAGE_UNSAFE"},
		{"unavailable", ports.ErrAgentAccountStorageUnavailable, http.StatusServiceUnavailable, "AGENT_ACCOUNT_STORAGE_UNAVAILABLE"},
		{"unsupported", ports.ErrAgentAccountStorageUnsupported, http.StatusNotImplemented, "AGENT_ACCOUNT_STORAGE_UNSUPPORTED"},
		{"credential store", ports.ErrAgentAccountCredentialStoreUnsupported, http.StatusNotImplemented, "AGENT_ACCOUNT_CREDENTIAL_STORE_UNSUPPORTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/accounts/login", nil)
			rec := httptest.NewRecorder()
			writeAgentAccountError(rec, req, fmt.Errorf("/private/fake-account/fake-secret-marker: %w", tc.err))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("response: %d %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "/private/") || strings.Contains(rec.Body.String(), "fake-secret-marker") {
				t.Fatal("storage error disclosed private data")
			}
		})
	}
}

func TestOccupiedUnavailableHomeExplainsSafeRecovery(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAgentAccountError(rec, httptest.NewRequest(http.MethodPost, "/", nil), fmt.Errorf("fake-secret-marker: %w", ports.ErrAgentAccountUnavailableHomeOccupied))
	for _, want := range []string{"account_unavailable_home_occupied", "Stop related child tools", ".unavailable", "only removes empty", `"retryable":true`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("missing diagnostic %q: %s", want, rec.Body.String())
		}
	}
	if strings.Contains(rec.Body.String(), "fake-secret-marker") {
		t.Fatal("private cause exposed")
	}
}
