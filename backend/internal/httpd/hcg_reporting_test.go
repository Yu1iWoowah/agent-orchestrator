package httpd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

func TestHCGReportingRequiresPrimaryNativeLoopback(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, nil, nil, APIDeps{}, ControlDeps{})
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/sessions/s/hcg-reporting"},
		{"PUT", "/api/v1/sessions/s/hcg-reporting/override"},
		{"DELETE", "/api/v1/sessions/s/hcg-reporting/override"},
		{"PUT", "/api/v1/hcg-reporting/emergency-stop"},
		{"PUT", "/api/v1/hcg-reporting/engine"},
		{"POST", "/api/v1/usage/sessions/s/report"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, "http://127.0.0.1"+route.path, nil)
			req.Host = "127.0.0.1"
			w := httptest.NewRecorder()
			lanControlBlock(router).ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("forged LAN Host reached control: %d", w.Code)
			}
			w = httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("primary route missing: %d", w.Code)
			}
			req.Header.Set("Origin", "https://untrusted.example")
			w = httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("browser reached control: %d", w.Code)
			}
		})
	}
}
