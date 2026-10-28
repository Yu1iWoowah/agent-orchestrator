package httpd

import (
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// Local filesystem locations are available to the exact desktop renderer origins
// and native clients, never to an arbitrary browser origin allowed on app APIs.
func documentReadOriginMiddleware(origins []string) func(http.Handler) http.Handler {
	allowed := exactAllowedOrigins(origins)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/local-documents" && !strings.HasPrefix(r.URL.Path, "/api/v1/local-documents/") {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Add("Vary", "Origin")
			w.Header().Add("Vary", "Sec-Fetch-Site")
			values := r.Header.Values("Origin")
			trusted := len(values) == 0 && r.Header.Get("Sec-Fetch-Site") == ""
			if len(values) == 1 && values[0] != "" {
				_, trusted = allowed[values[0]]
			}
			if !trusted {
				envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "ORIGIN_FORBIDDEN", "Origin is not allowed to resolve local document paths", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
