package httpd

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

func TestReplacementEndpointsRefuseWhenServiceUnavailable(t *testing.T) {
	router := NewRouterWithControl(config.Config{DataDir: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, APIDeps{}, ControlDeps{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/projects/demo/orchestrator/replacements"},
		{http.MethodGet, "/orchestrator/replacements/11111111-1111-4111-8111-111111111111"},
		{http.MethodPost, "/orchestrator/replacements/11111111-1111-4111-8111-111111111111/retry"},
		{http.MethodPost, "/orchestrator/replacements/11111111-1111-4111-8111-111111111111/cancel"},
		{http.MethodPost, "/orchestrator/replacements/11111111-1111-4111-8111-111111111111/prepared"},
		{http.MethodGet, "/orchestrator/replacements/11111111-1111-4111-8111-111111111111/intake"},
		{http.MethodGet, "/orchestrator/replacements/11111111-1111-4111-8111-111111111111/diagnose"},
		{http.MethodPost, "/orchestrator/replacements/11111111-1111-4111-8111-111111111111/repair"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(tc.method, "/api/v1"+tc.path, strings.NewReader("{}")))
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("unconfigured replacement service: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
