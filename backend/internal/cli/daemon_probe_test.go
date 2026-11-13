package cli

import (
	"context"
	"fmt"
	"net/http"
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
