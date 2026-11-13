package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

func TestDaemonConnectionIgnoresInvisiblePID(t *testing.T) {
	cfg := setReviewEnv(t)
	var reads, submits, probes atomic.Int32
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
			probes.Add(1)
			status := "ok"
			if r.URL.Path == "/readyz" {
				status = "ready"
			}
			_, _ = fmt.Fprintf(w, `{"status":%q,"service":%q,"pid":4242}`, status, daemonmeta.ServiceName)
		case "/api/v1/sessions":
			reads.Add(1)
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		case "/api/v1/sessions/worker/reviews/submit":
			submits.Add(1)
			_, _ = w.Write([]byte(`{"review":{"id":"run","verdict":"approved"}}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	if err := runfile.Write(cfg.runFile, runfile.Info{PID: 4242, Port: serverPort(t, srv.URL), StartedAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.runFile)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{ProcessAlive: func(int) bool { return false }}.withDefaults()
	c := newTestCommandContext(t, deps)
	var sessions struct{ Sessions []any }
	if err := c.getJSON(context.Background(), "sessions", &sessions); err != nil {
		t.Fatalf("reachable daemon read with invisible PID: %v", err)
	}
	var review reviewRunResponse
	if err := c.postReviewJSON(context.Background(), "sessions/worker/reviews/submit", submitReviewRequest{RunID: "run", Verdict: "approved"}, &review); err != nil {
		t.Fatalf("reachable daemon submit with invisible PID: %v", err)
	}
	out, _, err := executeCLI(t, deps, "status", "--json")
	if err != nil || !strings.Contains(out, `"state": "ready"`) {
		t.Fatalf("status = %s, err = %v", out, err)
	}
	if reads.Load() != 1 || submits.Load() != 1 || probes.Load() != 6 {
		t.Fatalf("requests: reads=%d submits=%d probes=%d", reads.Load(), submits.Load(), probes.Load())
	}
	after, err := os.ReadFile(cfg.runFile)
	if err != nil || string(after) != string(before) {
		t.Fatalf("connection observation changed run-file: %v", err)
	}
}

func TestDaemonAPIRedirectDoesNotReachSecondDestination(t *testing.T) {
	cfg := setReviewEnv(t)
	var second atomic.Int32
	other := newFixtureServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { second.Add(1) }))
	t.Cleanup(other.Close)
	var business atomic.Int32
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			status := map[string]string{"/healthz": "ok", "/readyz": "ready"}[r.URL.Path]
			_, _ = fmt.Fprintf(w, `{"status":%q,"service":%q,"pid":4242}`, status, daemonmeta.ServiceName)
			return
		}
		business.Add(1)
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	if err := runfile.Write(cfg.runFile, runfile.Info{PID: 4242, Port: serverPort(t, srv.URL), StartedAt: time.Unix(100, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	// The client may reach both owned servers, so only the redirect policy can
	// keep the body away from the second destination.
	first := rawFixtureClient(t, srv.Client()).Transport.(*testTargetGuard)
	bindTestHTTPFixture(t, other, 0)
	client := &http.Client{Transport: testFixtureClients{first, rawFixtureClient(t, other.Client()).Transport.(*testTargetGuard)}}
	c := newTestCommandContext(t, Deps{HTTPClient: client, ProcessAlive: func(int) bool { return false }})
	err := c.postReviewJSON(context.Background(), "sessions/worker/reviews/submit", submitReviewRequest{RunID: "run", Verdict: "approved"}, &reviewRunResponse{})
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirected submit = %v, want the 307 response reported", err)
	}
	if business.Load() != 1 || second.Load() != 0 {
		t.Fatalf("business=%d second=%d, want one attempt and no redirected request", business.Load(), second.Load())
	}
}

func TestDaemonHTTPClientIgnoresProxyAndKeepsSharedClient(t *testing.T) {
	var proxyCalls atomic.Int32
	shared := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
		proxyCalls.Add(1)
		return url.Parse("http://127.0.0.1:1")
	}}
	c := &commandContext{deps: Deps{HTTPClient: &http.Client{Transport: shared, Timeout: time.Minute}}}
	client := c.daemonHTTPClient(time.Second)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == shared || transport.Proxy != nil {
		t.Fatalf("daemon client transport = %#v, want a proxy-free clone", client.Transport)
	}
	if client.Timeout != time.Second || client.CheckRedirect == nil || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("daemon client must use its own timeout and never follow redirects")
	}
	if c.deps.HTTPClient.Transport != shared || shared.Proxy == nil || c.deps.HTTPClient.Timeout != time.Minute || c.deps.HTTPClient.CheckRedirect != nil {
		t.Fatal("daemon client changed the shared client used by stop")
	}
	if proxyCalls.Load() != 0 {
		t.Fatalf("proxy consulted %d times", proxyCalls.Load())
	}
}
