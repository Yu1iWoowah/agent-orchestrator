package httpd

import (
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// localOriginMiddleware requires an exact renderer origin or a native client
// for a sensitive route prefix, before CORS can grant a browser access.
func localOriginMiddleware(origins []string, prefix, message string) func(http.Handler) http.Handler {
	allowed := exactAllowedOrigins(origins)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
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
				envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "ORIGIN_FORBIDDEN", message, nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
