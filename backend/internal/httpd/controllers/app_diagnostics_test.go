package controllers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestAppDiagnosticsCannotBeEnabledOverHTTP(t *testing.T) {
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) { (&AppDiagnosticsController{}).Register(r) })
	for _, path := range []string{"grants", "enable", "consent"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/desktop/diagnostics/"+path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != 404 && rec.Code != 405 {
			t.Fatalf("HTTP minted grant at %s: %d", path, rec.Code)
		}
	}
}
func TestAppDiagnosticsRejectsBrowserAndNonLoopback(t *testing.T) {
	for _, tt := range []struct{ name, host, remote, header, value string }{{"origin", "127.0.0.1", "127.0.0.1:1", "Origin", "app://renderer"}, {"browser", "127.0.0.1", "127.0.0.1:1", "Sec-Fetch-Mode", "cors"}, {"lan", "127.0.0.1", "192.168.1.2:1", "", ""}, {"rebind", "evil.test", "127.0.0.1:1", "", ""}} {
		t.Run(tt.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Route("/api/v1", func(r chi.Router) { (&AppDiagnosticsController{}).Register(r) })
			req := httptest.NewRequest(http.MethodGet, "http://"+tt.host+"/api/v1/desktop/diagnostics/status", nil)
			req.RemoteAddr = tt.remote
			if tt.header != "" {
				req.Header.Set(tt.header, tt.value)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("unsafe caller not rejected: %d", rec.Code)
			}
		})
	}
}
