package httpd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
)

func TestReplacementRoutesAreRegisteredWithoutOptionalService(t *testing.T) {
	router := chi.NewRouter()
	httpd.NewAPI(config.Config{}, httpd.APIDeps{}).Register(router)
	id := "11111111-1111-4111-8111-111111111111"
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/projects/demo/orchestrator/replacements"},
		{http.MethodGet, "/api/v1/orchestrator/replacements/" + id},
		{http.MethodPost, "/api/v1/orchestrator/replacements/" + id + "/retry"},
		{http.MethodPost, "/api/v1/orchestrator/replacements/" + id + "/cancel"},
		{http.MethodPost, "/api/v1/orchestrator/replacements/" + id + "/prepared"},
	} {
		t.Run(request.method+request.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(request.method, request.path, strings.NewReader(`{}`)))
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("replacement route status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
