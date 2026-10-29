package httpd

import (
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// This retains untrusted claims for the settings service; it does not add
// authentication to the primary listener or assert that local input is human.
func settingsIdentityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"X-AO-Settings-Grant", "X-AO-Session-ID"} {
			values := r.Header.Values(name)
			if len(values) > 1 || len(values) == 1 && values[0] == "" || len(r.Header.Get(name)) > 8192 {
				envelope.WriteError(w, r, apierr.Forbidden("AGENT_SETTINGS_GRANT_INVALID", "Invalid settings caller identity"))
				return
			}
		}
		caller := ports.AgentSettingsCaller{SessionID: domain.SessionID(r.Header.Get("X-AO-Session-ID")), Grant: r.Header.Get("X-AO-Settings-Grant")}
		next.ServeHTTP(w, r.WithContext(ports.WithAgentSettingsCaller(r.Context(), caller)))
	})
}
