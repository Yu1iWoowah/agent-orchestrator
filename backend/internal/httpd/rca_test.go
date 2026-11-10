package httpd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

type rcaHTTPFixture struct {
	calls int
	fail  bool
}

func (s *rcaHTTPFixture) Get(context.Context, domain.SessionID, domain.RCAQuery) (domain.RCAComposition, error) {
	s.calls++
	if s.fail {
		return domain.RCAComposition{}, errors.New("A_SYNTHETIC_FAILURE B_CONTENT_FAILURE")
	}
	return domain.RCAComposition{Status: "unavailable", Reason: "disabled"}, nil
}
func (s *rcaHTTPFixture) SetOverride(context.Context, domain.SessionID, *bool) (domain.RCAPolicyView, error) {
	s.calls++
	return domain.RCAPolicyView{}, nil
}
func (s *rcaHTTPFixture) EmergencyStop(context.Context, bool) error     { s.calls++; return nil }
func (s *rcaHTTPFixture) Purge(context.Context, domain.SessionID) error { s.calls++; return nil }

func TestRCAControllerOriginLANValidationAndErrors(t *testing.T) {
	svc := &rcaHTTPFixture{}
	router := NewRouterWithControl(config.Config{AllowedOrigins: []string{"app://renderer", "http://localhost:5179"}}, discardLogger(), nil, APIDeps{RCA: svc}, ControlDeps{})
	path := "/api/v1/usage/sessions/selected/composition"
	request := func(method, path, origin, body string, lan bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		if lan {
			req.Header.Set("Authorization", "Bearer synthetic-lan-only")
			lanControlBlock(router).ServeHTTP(w, req)
		} else {
			router.ServeHTTP(w, req)
		}
		return w
	}
	for _, origin := range []string{"", "app://renderer", "http://localhost:5179"} {
		if request("GET", path, origin, "", false).Code != 200 {
			t.Fatal("trusted caller rejected")
		}
	}
	before := svc.calls
	for _, origin := range []string{"null", "http://localhost:5173", "https://preview.invalid"} {
		if request("GET", path, origin, "", false).Code != 403 {
			t.Fatal("untrusted origin accepted")
		}
	}
	if request("GET", "/api/v1/usage/sessions/encoded%2Fsegment/composition", "https://preview.invalid", "", false).Code != 403 {
		t.Fatal("escaped route bypassed origin gate")
	}
	for _, route := range []struct{ method, path string }{{"GET", path}, {"DELETE", path}, {"PUT", "/api/v1/sessions/selected/rca/override"}, {"DELETE", "/api/v1/sessions/selected/rca/override"}, {"PUT", "/api/v1/rca/emergency-stop"}} {
		if request(route.method, route.path, "app://renderer", `{"enabled":true}`, true).Code != 404 {
			t.Fatal("LAN spoof reached RCA")
		}
	}
	if svc.calls != before {
		t.Fatal("rejected caller had a side effect")
	}
	for _, body := range []string{`{}`, `null`, `{"enabled":null}`, `{"enabled":true,"raw":"A_SYNTHETIC_CREDENTIAL_CANARY"}`, `{"enabled":true} {}`} {
		response := request("PUT", "/api/v1/sessions/selected/rca/override", "", body, false)
		if response.Code != 400 || bytes.Contains(response.Body.Bytes(), []byte("A_SYNTHETIC")) {
			t.Fatal("invalid control request was reflected or accepted")
		}
	}
	for _, query := range []string{"?limit=0", "?path=A_SYNTHETIC", "?analyze=1", "?limit=1&limit=2", "?afterId=1"} {
		if request("GET", path+query, "", "", false).Code != 400 {
			t.Fatal("invalid composition paging accepted")
		}
	}
	svc.fail = true
	response := request("GET", path, "", "", false)
	if response.Code != 500 || bytes.Contains(response.Body.Bytes(), []byte("SYNTHETIC_FAILURE")) || bytes.Contains(response.Body.Bytes(), []byte("CONTENT_FAILURE")) {
		t.Fatal("raw error escaped controller")
	}
}

func TestRCARouteMatcherIsExact(t *testing.T) {
	for _, path := range []string{"/api/v1/usage/sessions/s/composition", "/api/v1/sessions/s/rca/override", "/api/v1/rca/emergency-stop", "/api/v1/rca/otlp/v1/logs"} {
		if !isRCAPath(path) || !isLANControlBlockedPath(path) {
			t.Fatalf("missing RCA boundary: %s", path)
		}
		if isRCAPath(path+"-unrelated") || isRCAPath(path+"/unrelated") {
			t.Fatal("logger exception too broad")
		}
	}
}

func TestRCARealMiddlewareSuppressesSinksAndCaptures(t *testing.T) {
	for _, canary := range []string{"A_SYNTHETIC_CREDENTIAL_CANARY", "B_SYNTHETIC_CONTENT_CANARY"} {
		for _, status := range []int{200, 400, 403, 409, 500} {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			sink := &captureSink{}
			httpCaptures, panicCaptures := 0, 0
			router := chi.NewRouter()
			router.Use(requestLoggerWithCapture(log, sink, func(context.Context, error, map[string]string, string) { httpCaptures++ }))
			router.Use(recoverTelemetryWithCapture(log, sink, func(context.Context, any, string, map[string]string, string) { panicCaptures++ }))
			router.Handle("/api/v1/usage/sessions/{sessionId}/composition", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == 500 {
					envelope.WriteError(w, r, errors.New(canary))
					return
				}
				w.WriteHeader(status)
			}))
			router.Get("/api/v1/rca/emergency-stop", func(http.ResponseWriter, *http.Request) { panic(canary) })
			router.Handle(rcaRawPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == 500 {
					envelope.WriteError(w, r, errors.New(canary))
					return
				}
				w.WriteHeader(status)
			}))
			router.Get("/control", func(w http.ResponseWriter, r *http.Request) { envelope.WriteError(w, r, errors.New("control failure")) })
			router.Get("/panic-control", func(http.ResponseWriter, *http.Request) { panic("control panic") })
			for _, path := range []string{"/api/v1/usage/sessions/s/composition", "/api/v1/rca/emergency-stop", rcaRawPath} {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if bytes.Contains(response.Body.Bytes(), []byte(canary)) {
					t.Fatal("RCA error response reflected content")
				}
			}
			if logs.Len() != 0 || len(sink.events) != 0 || httpCaptures != 0 || panicCaptures != 0 {
				t.Fatal("RCA diagnostics escaped exact exception")
			}
			for _, path := range []string{"/control", "/panic-control"} {
				router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
			}
			if logs.Len() == 0 || len(sink.events) == 0 || httpCaptures == 0 || panicCaptures == 0 {
				t.Fatal("non-RCA controls did not exercise real exits")
			}
		}
	}
}
