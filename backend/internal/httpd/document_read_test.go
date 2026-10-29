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

func TestDocumentReadRoutesAreRegistered(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, APIDeps{}, ControlDeps{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/local-documents/resolve"},
		{http.MethodPost, "/api/v1/local-documents/content"},
		{http.MethodGet, "/api/v1/projects/project/document-catalog"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"kind":"shared","projectId":"project","path":"doc.md"}`)))
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("missing optional read service: status=%d body=%s; want 501", w.Code, w.Body.String())
			}
		})
	}
}

func TestDocumentResolverNeverReachesLANOrTunnelHandler(t *testing.T) {
	calls := 0
	h := lanControlBlock(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusOK) }))
	for _, path := range []string{"/api/v1/local-documents/resolve", "/api/v1/local-documents/content", "/api/v1/local%2Ddocuments/content", "/api/v1/local-documents%2Fcontent", "/api/v1/local-documents", "/api/v1/local-documents/resolve/child", "/api/v1/local%2Ddocuments/resolve", "/api/v1/local-documents%2Fresolve"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Host = "127.0.0.1"
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Origin", "app://renderer")
		req.Header.Set("Authorization", "Bearer valid-local-password")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s status=%d want404", path, w.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("resolver reached authenticated LAN/tunnel handler %d times", calls)
	}
}

func TestDocumentResolverRequiresExactRendererOrigin(t *testing.T) {
	router := NewRouterWithControl(config.Config{AllowedOrigins: []string{"app://renderer"}}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, APIDeps{}, ControlDeps{})
	for _, path := range []string{"/api/v1/local-documents/resolve", "/api/v1/local-documents/content"} {
		for _, tc := range []struct {
			origin, site string
			want         int
		}{{"https://evil.example", "", 403}, {"null", "", 403}, {"", "cross-site", 403}, {"app://renderer", "same-origin", 501}, {"", "", 501}} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("origin=%q site=%q status=%d want%d", tc.origin, tc.site, w.Code, tc.want)
			}
		}
	}
}
