package httpd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
	guardiansvc "github.com/aoagents/agent-orchestrator/backend/internal/service/guardian"
)

type guardianQueryFake struct{ reads, purges int }

func (f *guardianQueryFake) Query(_ context.Context, id domain.SessionID, _ guardiansvc.QueryOptions) (guardiansvc.Report, error) {
	f.reads++
	if id == "missing" {
		return guardiansvc.Report{}, apierr.NotFound("SESSION_NOT_FOUND", "Session not found.")
	}
	return guardiansvc.Report{SchemaVersion: 1, Coverage: "partial"}, nil
}
func (f *guardianQueryFake) Purge(_ context.Context, id domain.SessionID) error {
	f.purges++
	if id == "missing" {
		return apierr.NotFound("SESSION_NOT_FOUND", "Session not found.")
	}
	return nil
}
func TestA10GuardianAPITransportAndValidation(t *testing.T) {
	fake := &guardianQueryFake{}
	router := NewRouterWithControl(config.Config{AllowedOrigins: []string{"app://renderer"}}, discardLogger(), nil, APIDeps{GuardianObservability: fake}, ControlDeps{})
	const path = "/api/v1/sessions/session/guardian-observability"
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		for _, remote := range []bool{false, true} {
			for _, origin := range []string{"", "app://renderer", "https://example.test"} {
				t.Run(method+origin+map[bool]string{true: "/LAN", false: "/local"}[remote], func(t *testing.T) {
					req := httptest.NewRequest(method, "http://127.0.0.1"+path, nil)
					if origin != "" {
						req.Header.Set("Origin", origin)
					}
					w := httptest.NewRecorder()
					var handler http.Handler = router
					if remote {
						handler = lanControlBlock(router)
					}
					handler.ServeHTTP(w, req)
					want := http.StatusOK
					if remote {
						want = http.StatusNotFound
					} else if origin != "" {
						want = http.StatusForbidden
					}
					if w.Code != want {
						t.Fatalf("got %d want %d: %s", w.Code, want, w.Body.String())
					}
				})
			}
		}
	}
	if fake.reads != 1 || fake.purges != 1 {
		t.Fatalf("gate called service: %+v", fake)
	}
	for _, suffix := range []string{"?since=invalid", "?since=2026-01-01T00:00:00%2B08:00", "?details=maybe", "?limit=0", "?details=true&limit=501", "?cursor=abc", "?details=true&details=false", "?unknown=secret"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path+suffix, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "requestId") {
			t.Fatalf("bad validation %s: %d %s", suffix, w.Code, w.Body.String())
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, "http://127.0.0.1/api/v1/sessions/missing/guardian-observability", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "SESSION_NOT_FOUND") || !strings.Contains(w.Body.String(), "requestId") {
			t.Fatalf("missing envelope: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestA10GuardianTunnelBlocksMetadataEvenWithLoopbackHost(t *testing.T) {
	fake := &guardianQueryFake{}
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{GuardianObservability: fake}, ControlDeps{})
	state := &authState{}
	state.setHash(mobilebridge.HashPassword("secret12"))
	manager := NewLANManager(router, state, 0, discardLogger(), nil)
	port, err := manager.StartLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	ws, _, err := websocket.Dial(context.Background(), fmt.Sprintf("ws://127.0.0.1:%d%s", port, lanTunnelPath), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer secret12"}}})
	if err != nil {
		t.Fatal(err)
	}
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	defer conn.Close()
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{Protocols: protocols, DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, err := http.NewRequest(method, "http://127.0.0.1/api/v1/sessions/session/guardian-observability", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer secret12")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("tunnel reached metadata: %d", response.StatusCode)
		}
	}
	if fake.reads != 0 || fake.purges != 0 {
		t.Fatalf("tunnel invoked service: %+v", fake)
	}
}
