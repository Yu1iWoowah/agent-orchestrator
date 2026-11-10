package httpd

import (
	"net"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// Only the actual fixed query/control routes receive the diagnostic exception,
// independent of method/status so rejection and off/5xx cannot bypass it.
func isRCAPath(path string) bool {
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	if path == "/api/v1/rca/emergency-stop" || path == "/api/v1/rca/emergency-stop/" {
		return true
	}
	if len(parts) == 7 && parts[0] == "" && parts[1] == "api" && parts[2] == "v1" {
		return (parts[3] == "usage" && parts[4] == "sessions" && parts[5] != "" && parts[6] == "composition") || (parts[3] == "sessions" && parts[4] != "" && parts[5] == "rca" && parts[6] == "override")
	}
	return false
}

func isRCARequest(r *http.Request) bool {
	return isRCAPath(r.URL.Path) || isRCAPath(r.URL.EscapedPath())
}

// Fixed status-class counters are the entire request diagnostic footprint.
var rcaRequests [6]atomic.Uint64

func rcaOriginMiddleware(origins []string) func(http.Handler) http.Handler {
	allowed := exactAllowedOrigins(origins)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isRCARequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Add("Vary", "Origin")
			w.Header().Add("Vary", "Sec-Fetch-Site")
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			ip := net.ParseIP(host)
			local := host == "localhost" || (ip != nil && ip.IsLoopback())
			values := r.Header.Values("Origin")
			trusted := len(values) == 0
			if len(values) == 0 {
				for _, header := range []string{"Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-User", "Referer"} {
					if r.Header.Get(header) != "" {
						trusted = false
					}
				}
			} else if len(values) == 1 && values[0] != "" {
				_, trusted = allowed[values[0]]
			} else {
				trusted = false
			}
			if !local || !trusted {
				envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "RCA_ORIGIN_FORBIDDEN", "Trusted renderer or native loopback access is required", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
