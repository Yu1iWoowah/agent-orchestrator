package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSettingsIdentityMiddlewareKeepsClaimsAndRejectsAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		status int
	}{{"absent", nil, 204}, {"valid", []string{"secret"}, 204}, {"blank", []string{""}, 403}, {"duplicates", []string{"secret", "second"}, 403}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPatch, "/api/v1/sessions/worker/conversation/settings", nil)
			if tc.values != nil {
				r.Header["X-Ao-Settings-Grant"] = tc.values
				r.Header.Set("X-AO-Session-ID", "orch")
			}
			w := httptest.NewRecorder()
			settingsIdentityMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				caller := ports.AgentSettingsCallerFromContext(r.Context())
				if tc.name == "valid" && (caller.SessionID != "orch" || caller.Grant != "secret") {
					t.Error("legacy route lost identity")
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(w, r)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
