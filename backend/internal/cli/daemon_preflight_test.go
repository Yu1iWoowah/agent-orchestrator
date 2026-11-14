package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

func TestInvalidRunFileStopsBeforeRequest(t *testing.T) {
	cases := []struct {
		name                 string
		pid, port            int
		fixturePort, missing bool
		raw, want            string
	}{
		{name: "missing", missing: true, want: "no run-file — start it with `ao start`"},
		{name: "malformed JSON", raw: "{", want: "parse run-file"},
		{name: "noninteger PID", raw: "{\"pid\":1.5,\"port\":1}", want: "parse run-file"},
		{name: "zero PID", fixturePort: true, want: "invalid daemon run-file"},
		{name: "negative PID", pid: -1, fixturePort: true, want: "invalid daemon run-file"},
		{name: "zero port", pid: 4242, want: "invalid daemon run-file"},
		{name: "negative port", pid: 4242, port: -1, want: "invalid daemon run-file"},
		{name: "overflow port", pid: 4242, port: 65536, want: "invalid daemon run-file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setReviewEnv(t)
			var requests atomic.Int32
			srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			}))
			t.Cleanup(srv.Close)
			bindTestHTTPFixture(t, srv, 4242)
			var before []byte
			if !tc.missing {
				port := tc.port
				if tc.fixturePort {
					port = serverPort(t, srv.URL)
				}
				var err error
				before, err = json.Marshal(runfile.Info{PID: tc.pid, Port: port})
				if err != nil {
					t.Fatal(err)
				}
				if tc.raw != "" {
					before = []byte(tc.raw)
				}
				if err := os.WriteFile(cfg.runFile, before, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c := newTestCommandContext(t, Deps{HTTPClient: rawFixtureClient(t, srv.Client()), ProcessAlive: func(int) bool { return false }})
			err := c.postReviewJSON(context.Background(), "sessions/worker/reviews/submit", submitReviewRequest{RunID: "run", Verdict: "approved"}, &reviewRunResponse{})
			var attempted daemonRequestError
			if err == nil || errors.As(err, &attempted) || strings.Contains(err.Error(), "unconfirmed") {
				t.Fatalf("preflight result = %v", err)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if tc.missing && !errors.Is(err, errDaemonNotRunning) {
				t.Fatalf("missing locator classification = %v", err)
			}
			if requests.Load() != 0 {
				t.Fatalf("requests = %d, want zero", requests.Load())
			}
			after, readErr := os.ReadFile(cfg.runFile)
			if tc.missing {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("missing run-file changed: %v", readErr)
				}
			} else if readErr != nil || string(after) != string(before) {
				t.Fatalf("run-file changed: %v", readErr)
			}
		})
	}
}

func TestProbeRejectionStopsBeforeBody(t *testing.T) {
	goodHealth := fmt.Sprintf("{\"status\":\"ok\",\"service\":%q,\"pid\":4242}", daemonmeta.ServiceName)
	goodReady := fmt.Sprintf("{\"status\":\"ready\",\"service\":%q,\"pid\":4242}", daemonmeta.ServiceName)
	cases := []struct {
		name, health, ready, want string
		healthStatus, readyStatus int
	}{
		{name: "health wrong service", health: "{\"status\":\"ok\",\"service\":\"foreign\",\"pid\":4242}", want: string(stateStale)},
		{name: "health wrong PID", health: fmt.Sprintf("{\"status\":\"ok\",\"service\":%q,\"pid\":4243}", daemonmeta.ServiceName), want: string(stateStale)},
		{name: "health malformed", health: "{", want: string(stateUnhealthy)},
		{name: "health missing status", health: fmt.Sprintf("{\"service\":%q,\"pid\":4242}", daemonmeta.ServiceName), want: string(stateUnhealthy)},
		{name: "health trailing JSON", health: goodHealth + " {}", want: string(stateUnhealthy)},
		{name: "health HTTP failure", healthStatus: 503, want: string(stateUnhealthy)},
		{name: "health redirect", healthStatus: 302, want: string(stateUnhealthy)},
		{name: "ready wrong service", ready: "{\"status\":\"ready\",\"service\":\"foreign\",\"pid\":4242}", want: string(stateStale)},
		{name: "ready wrong PID", ready: fmt.Sprintf("{\"status\":\"ready\",\"service\":%q,\"pid\":4243}", daemonmeta.ServiceName), want: string(stateStale)},
		{name: "not ready", ready: fmt.Sprintf("{\"status\":\"starting\",\"service\":%q,\"pid\":4242}", daemonmeta.ServiceName), want: string(stateNotReady)},
		{name: "ready malformed", ready: "{", want: string(stateNotReady)},
		{name: "ready HTTP failure", readyStatus: 503, want: string(stateNotReady)},
		{name: "ready redirect", readyStatus: 302, want: string(stateNotReady)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setReviewEnv(t)
			var business atomic.Int32
			srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, code := tc.health, tc.healthStatus
				switch r.URL.Path {
				case "/healthz":
					if body == "" {
						body = goodHealth
					}
				case "/readyz":
					body, code = tc.ready, tc.readyStatus
					if body == "" {
						body = goodReady
					}
				default:
					business.Add(1)
					http.Error(w, "business request forbidden", http.StatusInternalServerError)
					return
				}
				if code == 0 {
					code = http.StatusOK
				}
				if code >= 300 && code < 400 {
					w.Header().Set("Location", "/api/v1/leak")
				}
				w.WriteHeader(code)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)
			bindTestHTTPFixture(t, srv, 4242)
			if err := runfile.Write(cfg.runFile, runfile.Info{PID: 4242, Port: serverPort(t, srv.URL)}); err != nil {
				t.Fatal(err)
			}
			c := newTestCommandContext(t, Deps{HTTPClient: rawFixtureClient(t, srv.Client()), ProcessAlive: func(int) bool { return true }})
			err := c.postReviewJSON(context.Background(), "sessions/worker/reviews/submit", submitReviewRequest{RunID: "run", Verdict: "approved"}, &reviewRunResponse{})
			var attempted daemonRequestError
			if err == nil || errors.As(err, &attempted) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "unconfirmed") {
				t.Fatalf("probe rejection = %v, want %s before attempt", err, tc.want)
			}
			if business.Load() != 0 {
				t.Fatalf("business requests = %d, want zero", business.Load())
			}
		})
	}
}

func TestPreflightTransportErrorPreservesCause(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		ready bool
	}{
		{name: "permission denied", cause: syscall.EACCES},
		{name: "connection refused", cause: syscall.ECONNREFUSED},
		{name: "health timeout", cause: context.DeadlineExceeded},
		{name: "ready timeout", cause: context.DeadlineExceeded, ready: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setReviewEnv(t)
			srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("terminal failure reached network") }))
			t.Cleanup(srv.Close)
			bindTestHTTPFixture(t, srv, 4242)
			if err := runfile.Write(cfg.runFile, runfile.Info{PID: 4242, Port: serverPort(t, srv.URL)}); err != nil {
				t.Fatal(err)
			}
			client := terminalErrorClient(tc.cause)
			if tc.ready {
				client = rawFixtureClient(t, &http.Client{Transport: &testTerminalResponse{respond: func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodGet && req.URL.Path == "/healthz" {
						return jsonResponse(http.StatusOK, fmt.Sprintf("{\"status\":\"ok\",\"service\":%q,\"pid\":4242}", daemonmeta.ServiceName)), nil
					}
					return nil, tc.cause
				}}})
			}
			c := newTestCommandContext(t, Deps{HTTPClient: client, ProcessAlive: func(int) bool { return false }})
			err := c.postReviewJSON(context.Background(), "sessions/worker/reviews/submit", submitReviewRequest{RunID: "run", Verdict: "approved"}, &reviewRunResponse{})
			var attempted daemonRequestError
			if !errors.Is(err, tc.cause) || errors.Is(err, errDaemonNotRunning) || errors.As(err, &attempted) || strings.Contains(err.Error(), "unconfirmed") {
				t.Fatalf("transport classification = %v, cause %v", err, tc.cause)
			}
			if tc.ready && !strings.Contains(err.Error(), string(stateNotReady)) {
				t.Fatalf("ready timeout classification = %v", err)
			}
		})
	}
}
