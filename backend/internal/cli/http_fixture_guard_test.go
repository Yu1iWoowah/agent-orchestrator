package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

func TestFixtureGuardRejectsUnownedTargets(t *testing.T) {
	var handlerCalls, observerCalls, delegateCalls atomic.Int32
	owned := newFixtureServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerCalls.Add(1)
	}))
	bindTestHTTPFixture(t, owned, 4242)
	client := observingFixtureClient(t, &http.Client{Transport: &testTerminalResponse{respond: func(*http.Request) (*http.Response, error) {
		delegateCalls.Add(1)
		return jsonResponse(http.StatusOK, `{}`), nil
	}}}, true, func(req *http.Request, next http.RoundTripper) (*http.Response, error) {
		observerCalls.Add(1)
		return next.RoundTrip(req)
	})
	peer := newFixtureServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerCalls.Add(1)
	}))
	if owned.URL == "http://127.0.0.1:3001" || peer.URL == "http://127.0.0.1:3001" {
		t.Fatal("unknown-target case requires neither owned listener to use port 3001")
	}
	for _, origin := range []string{
		"http://127.0.0.1:3001", peer.URL,
		strings.Replace(owned.URL, "127.0.0.1", "localhost", 1),
		strings.Replace(owned.URL, "http://", "https://", 1),
	} {
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/healthz"}, {http.MethodGet, "/readyz"},
			{http.MethodPost, "/api/v1/sessions/fixture/activity"}, {http.MethodPost, "/shutdown"},
		} {
			req, err := http.NewRequestWithContext(t.Context(), route.method, origin+route.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := client.Transport.RoundTrip(req); err == nil || resp != nil {
				t.Fatalf("unowned target accepted: %s %s response=%v error=%v", route.method, req.URL, resp, err)
			}
		}
	}
	if observerCalls.Load() != 0 || delegateCalls.Load() != 0 || handlerCalls.Load() != 0 {
		t.Fatalf("unowned target reached lower layer: observer=%d delegate=%d handler=%d", observerCalls.Load(), delegateCalls.Load(), handlerCalls.Load())
	}
}

func TestFixtureObserverPreservesCapturedScope(t *testing.T) {
	for _, scopeKind := range []string{"single", "group"} {
		t.Run(scopeKind, func(t *testing.T) {
			var observerCalls, delegateCalls atomic.Int32
			terminal := &http.Client{Transport: &testTerminalResponse{respond: func(*http.Request) (*http.Response, error) {
				delegateCalls.Add(1)
				return jsonResponse(http.StatusOK, `{}`), nil
			}}}
			owned := newFixtureServer(t, http.NotFoundHandler())
			saved := rawFixtureClient(t, terminal)
			origins := []string{owned.URL}
			if scopeKind == "group" {
				second := newFixtureServer(t, http.NotFoundHandler())
				secondClient := rawFixtureClient(t, terminal)
				saved = &http.Client{Transport: testFixtureClients{
					saved.Transport.(*testTargetGuard), secondClient.Transport.(*testTargetGuard),
				}}
				origins = append(origins, second.URL)
			}
			peer := newFixtureServer(t, http.NotFoundHandler())
			client := observingFixtureClient(t, saved, false, func(req *http.Request, next http.RoundTripper) (*http.Response, error) {
				observerCalls.Add(1)
				return next.RoundTrip(req)
			})
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, peer.URL+"/api/v1/sessions/fixture/activity", nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := client.Transport.RoundTrip(req); err == nil || resp != nil {
				t.Fatalf("ambient peer accepted: response=%v error=%v", resp, err)
			}
			if observerCalls.Load() != 0 || delegateCalls.Load() != 0 {
				t.Fatalf("ambient peer entered observer or delegate: observer=%d delegate=%d", observerCalls.Load(), delegateCalls.Load())
			}
			for _, origin := range origins {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/api/v1/sessions/fixture/activity", nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.Transport.RoundTrip(req)
				if err != nil {
					t.Fatalf("captured origin rejected: %s: %v", origin, err)
				}
				_ = resp.Body.Close()
			}
			if observerCalls.Load() != int32(len(origins)) || delegateCalls.Load() != int32(len(origins)) {
				t.Fatalf("captured scope changed: observer=%d delegate=%d origins=%d", observerCalls.Load(), delegateCalls.Load(), len(origins))
			}
		})
	}
}

func TestFixtureGuardOwnListenerAndClosedLifecycle(t *testing.T) {
	var handlerCalls, observerCalls atomic.Int32
	server := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls.Add(1)
		_, _ = io.WriteString(w, "original handler")
	}))
	bindTestHTTPFixture(t, server, 4242)
	client := observingFixtureClient(t, server.Client(), false, func(req *http.Request, next http.RoundTripper) (*http.Response, error) {
		observerCalls.Add(1)
		return next.RoundTrip(req)
	})
	resp, err := client.Get(server.URL + "/api/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || string(body) != "original handler" || handlerCalls.Load() != 1 || observerCalls.Load() != 1 {
		t.Fatalf("owned listener changed handler: body=%q error=%v handler=%d observer=%d", body, readErr, handlerCalls.Load(), observerCalls.Load())
	}
	apiClient := apiFixtureClient(t, server.Client())
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := apiClient.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if handlerCalls.Load() != 1 {
		t.Fatal("API probe synthesis reached the business handler")
	}
	server.Close()
	for _, path := range []string{"/healthz", "/readyz", "/api/v1/sessions", "/shutdown"} {
		for _, guarded := range []*http.Client{client, apiClient} {
			if resp, err := guarded.Get(server.URL + path); err == nil || resp != nil {
				t.Fatalf("closed listener accepted %s: response=%v error=%v", path, resp, err)
			}
		}
	}
	if handlerCalls.Load() != 1 || observerCalls.Load() != 1 {
		t.Fatalf("closed listener reached lower layer: handler=%d observer=%d", handlerCalls.Load(), observerCalls.Load())
	}
}

// This candidate uses no listener. Only fixture-local run-file data is written;
// it exercises doctor HTTP inspection rather than the full doctor command.
func TestFixtureGuardRawEntryPoints(t *testing.T) {
	cfg := setConfigEnv(t)
	if err := runfile.Write(cfg.runFile, runfile.Info{PID: os.Getpid(), Port: 3001, StartedAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	var delegateCalls atomic.Int32
	deps := Deps{
		HTTPClient: &http.Client{Transport: &testTerminalResponse{respond: func(*http.Request) (*http.Response, error) {
			delegateCalls.Add(1)
			return jsonResponse(http.StatusOK, `{}`), nil
		}}},
		ProcessAlive: func(int) bool { return true }, Out: io.Discard, Err: io.Discard,
	}
	c := newTestCommandContext(t, deps)
	if err := c.getJSON(context.Background(), "sessions", new(any)); err == nil {
		t.Fatal("direct command context accepted an unowned target")
	}
	if err := c.requestShutdown(t.Context(), 3001); err == nil {
		t.Fatal("direct shutdown accepted an unowned target")
	}
	if _, err := c.inspectDaemon(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCLI(t, deps, "status", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCLI(t, deps, "stop", "--json"); err == nil {
		t.Fatal("stop accepted an unowned target")
	}
	root := newTestRootCommand(t, deps)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if err := executeWithTestDeps(t, deps, []string{"doctor", "extra"}); ExitCode(err) != 2 {
		t.Fatalf("direct execution must retain usage error: %v", err)
	}
	if delegateCalls.Load() != 0 {
		t.Fatalf("raw, shutdown, doctor inspection or root telemetry delegated %d requests", delegateCalls.Load())
	}
}

func TestFixtureGuardRejectsForbiddenDialTargets(t *testing.T) {
	server := newFixtureServer(t, http.NotFoundHandler())
	fixture := currentTestHTTPFixture(t)
	base, ok := http.DefaultTransport.(*http.Transport)
	if fixture == nil || !ok {
		t.Fatal("fixture server and default transport are required")
	}
	dial := fixture.transport(base).DialContext
	owned := strings.TrimPrefix(server.URL, "http://")
	for _, target := range []struct{ network, address string }{
		{"tcp", "127.0.0.1:3001"}, {"tcp", "localhost:" + strings.Split(owned, ":")[1]},
		{"tcp", "[::1]:" + strings.Split(owned, ":")[1]}, {"tcp", "127.0.0.1:1"}, {"unix", owned},
	} {
		if conn, err := dial(t.Context(), target.network, target.address); err == nil {
			_ = conn.Close()
			t.Fatalf("fixture dialed forbidden target %s %s", target.network, target.address)
		}
	}
	for _, address := range []string{"127.0.0.1:3001", "localhost:80", "[::1]:3002", "127.0.0.1:0", "127.0.0.1"} {
		if fixtureAddressAllowed(address) {
			t.Fatalf("forbidden fixture address allowed: %s", address)
		}
	}
	conn, err := dial(t.Context(), "tcp", owned)
	if err != nil {
		t.Fatalf("owned listener rejected: %v", err)
	}
	_ = conn.Close()
	server.Close()
	if conn, err := dial(t.Context(), "tcp", owned); err == nil {
		_ = conn.Close()
		t.Fatal("closed listener was dialed")
	}
}
