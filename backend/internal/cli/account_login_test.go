package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestLoginWaitsForNativeExitBeforeVerifyAndRetriesTransientFailure(t *testing.T) {
	for _, reconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("reconnect=%t", reconnect), func(t *testing.T) {
			cfg := setConfigEnv(t)
			var exited atomic.Bool
			var verifies atomic.Int32
			observed := make(chan struct{})
			premature := make(chan struct{}, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			finish := func() { releaseOnce.Do(func() { close(release) }) }
			defer finish()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/telemetry/") {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				switch r.URL.Path {
				case "/api/v1/agents/accounts/login":
					_, _ = io.WriteString(w, `{"operationId":"op","shellTerminal":{"handleId":"native-login"}}`)
				case "/api/v1/agents/accounts/operations/op":
					phase, outcome := "pending", ""
					if verifies.Load() >= 2 {
						phase, outcome = "complete", "complete"
					}
					_, _ = fmt.Fprintf(w, `{"operationId":"op","action":"login","phase":%q,"outcome":%q,"resultAccountId":"A","terminalHandleId":"native-login"}`, phase, outcome)
				case "/api/v1/agents/accounts/login/op/verify":
					if !exited.Load() {
						select {
						case premature <- struct{}{}:
						default:
						}
					}
					if verifies.Add(1) == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = io.WriteString(w, `{"code":"AGENT_ACCOUNT_PROVIDER_UNAVAILABLE","message":"Retry later","requestId":"transient"}`)
						return
					}
					_, _ = io.WriteString(w, `{"status":"complete"}`)
				case "/mux":
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.CloseNow() }()
					var open accountTerminalFrame
					if err = wsjson.Read(ctx, conn, &open); err != nil {
						return
					}
					close(observed)
					_ = wsjson.Write(ctx, conn, accountTerminalFrame{Ch: "terminal", Type: "data", ID: open.ID, Data: base64.StdEncoding.EncodeToString([]byte("access_token=PRIVATE_TOKEN_CANARY"))})
					select {
					case <-release:
					case <-ctx.Done():
						return
					}
					exited.Store(true)
					_ = wsjson.Write(ctx, conn, accountTerminalFrame{Ch: "terminal", Type: "exited", ID: open.ID})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			var out, stderr bytes.Buffer
			cmd := NewRootCommand(Deps{Out: &out, Err: &stderr, ProcessAlive: func(int) bool { return true }})
			args := []string{"account", "add", "--provider", "codex", "--no-attach", "--json"}
			if reconnect {
				args = []string{"account", "login", "--operation", "op", "--no-attach", "--json"}
			}
			cmd.SetArgs(args)
			done := make(chan error, 1)
			go func() { done <- cmd.ExecuteContext(ctx) }()
			select {
			case <-premature:
				finish()
				<-done
				t.Fatal("credentials were accepted while native login was still running")
			case <-observed:
			case err := <-done:
				t.Fatalf("wait ended before observing exit: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case <-premature:
				finish()
				<-done
				t.Fatal("verify raced the native credential writer")
			case <-time.After(600 * time.Millisecond):
			}
			if verifies.Load() != 0 {
				t.Fatal("native terminal would have been closed before exit")
			}
			finish()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("transient verification ended wait: %v", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if verifies.Load() != 2 || strings.Contains(out.String()+stderr.String(), "TOKEN_CANARY") || !strings.Contains(out.String(), `"outcome": "complete"`) {
				t.Fatalf("verify=%d output=%s stderr=%s", verifies.Load(), out.String(), stderr.String())
			}
		})
	}
}

func TestLoginOutputShowsPromptEchoAndFinalFragmentWithoutNewline(t *testing.T) {
	var out bytes.Buffer
	var output accountLoginOutput
	for _, part := range []string{"Paste authorization code: ", "ABCD-1234", "\r\n", "Final step complete"} {
		before := out.String()
		output.write(&out, []byte(part))
		if out.String() != before+part {
			t.Fatalf("output was delayed: got %q want suffix %q", out.String(), part)
		}
	}
}

func TestAccountWaitStopsImmediatelyForUnconfirmedTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := accountTransitionResponse{Transition: &accountTransition{ID: "t", Phase: "target_starting", ErrorCode: "TARGET_STOP_UNCONFIRMED"}}
	err := (&commandContext{}).waitAccountTransition(ctx, "s", &result)
	if err == nil || !strings.Contains(err.Error(), "Stop and retry") || ctx.Err() != nil {
		t.Fatalf("unconfirmed target was not actionable immediately: %v", err)
	}
}

func serveExitedAccountLogin(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	var open accountTerminalFrame
	if err = wsjson.Read(ctx, conn, &open); err != nil {
		t.Error(err)
		return
	}
	if err = wsjson.Write(ctx, conn, accountTerminalFrame{Ch: "terminal", Type: "exited", ID: open.ID}); err != nil {
		t.Error(err)
	}
}

func TestLoginOutputMasksCredentialParameterClassesBeforeValueAcrossFrames(t *testing.T) {
	const value = "Z9_PRIVATE_TOKEN_CANARY"
	for name, line := range map[string]string{
		"token":           "token=" + value + "\r\n",
		"client-secret":   "client_secret=" + value + "\r\n",
		"derived-token":   "session_OAuthToken = " + value + "\r\n",
		"json-secret":     "{\"clientSecret\":\"" + value + "\"}\r\n",
		"password":        "PASSWORD: " + value + "\r\n",
		"private-key":     "private_key=" + value + "\r\n",
		"credential":      "providerCredential=" + value + "\r\n",
		"url":             "https://example.test/?token=" + value + "\r\n",
		"encoded-url-key": "https://example.test/?to%6ben=" + value + "\r\n",
		"nested-index":    "access_token[session]=" + value + "\r\n",
		"indexed-url":     "https://example.test/?refresh_token[0]=" + value + "\r\n",
		"dotted-token":    "oauth.token.value=" + value + "\r\n",
		"quoted-index":    "oauth[\"token\"][\"value\"]=" + value + "\r\n",
		"nested-key":      "private_key[session]=" + value + "\r\n",
		"nested-auth":     "authorization.scope=" + value + "\r\n",
		"ansi":            "to\x1b[32mken\x1b[0m=" + value + "\r\n",
		"osc":             "\x1b]8;;https://example.test/?token=" + value + "\x1b\\Sign in\x1b]8;;\x1b\\\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			var bytesOut bytes.Buffer
			var bytesOutput accountLoginOutput
			for i := range len(line) {
				bytesOutput.write(&bytesOut, []byte(line[i:i+1]))
				if strings.Contains(bytesOut.String(), "Z") {
					t.Fatalf("credential prefix escaped in byte stream at %d", i)
				}
			}
			for split := 0; split <= len(line); split++ {
				var out bytes.Buffer
				var output accountLoginOutput
				output.write(&out, []byte(line[:split]))
				if strings.Contains(out.String(), "Z") {
					t.Fatalf("credential prefix escaped before complete value, split=%d", split)
				}
				output.write(&out, []byte(line[split:]))
				if strings.Contains(out.String(), "Z") {
					t.Fatalf("credential value escaped, split=%d", split)
				}
			}
		})
	}
}

func TestLoginAfterExitEndsUnsuccessfulVerificationWithReconnectInstructions(t *testing.T) {
	for _, tc := range []struct {
		name, code, message string
		status, calls       int
		daemonStopped       bool
	}{
		{name: "not signed in", status: http.StatusOK, message: "not signed in", calls: 1},
		{name: "temporary unavailable", status: http.StatusServiceUnavailable, code: "AGENT_ACCOUNT_PROVIDER_UNAVAILABLE", message: "Retry later", calls: 3},
		{name: "storage needs repair", status: http.StatusServiceUnavailable, code: "AGENT_ACCOUNT_STORAGE_UNAVAILABLE", message: "Check account storage permissions", calls: 1},
		{name: "storage unsupported", status: http.StatusNotImplemented, code: "AGENT_ACCOUNT_STORAGE_UNSUPPORTED", message: "Profiles are not supported", calls: 1},
		{name: "daemon stopped", daemonStopped: true, message: "ao start", calls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			var verifies, cancellations atomic.Int32
			var alive atomic.Bool
			alive.Store(true)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/telemetry/") {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				switch r.URL.Path {
				case "/api/v1/agents/accounts/operations/op":
					_, _ = io.WriteString(w, `{"operationId":"op","action":"login","phase":"pending","terminalHandleId":"native-login"}`)
				case "/mux":
					if tc.daemonStopped {
						alive.Store(false)
					}
					serveExitedAccountLogin(t, w, r)
				case "/api/v1/agents/accounts/login/op/verify":
					verifies.Add(1)
					w.WriteHeader(tc.status)
					if tc.status == http.StatusOK {
						_, _ = io.WriteString(w, `{"status":"waiting"}`)
					} else {
						_, _ = fmt.Fprintf(w, `{"code":%q,"message":%q,"requestId":"verify-request"}`, tc.code, tc.message)
					}
				case "/api/v1/agents/accounts/login/op/cancel":
					cancellations.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var out, stderr bytes.Buffer
			cmd := NewRootCommand(Deps{Out: &out, Err: &stderr, ProcessAlive: func(int) bool { return alive.Load() }})
			cmd.SetArgs([]string{"account", "login", "--operation", "op", "--no-attach"})
			err := cmd.ExecuteContext(ctx)
			if err == nil || ctx.Err() != nil || verifies.Load() != int32(tc.calls) || cancellations.Load() != 0 {
				t.Fatalf("login did not finish independently of cancellation: calls=%d cancels=%d ctx=%v err=%v", verifies.Load(), cancellations.Load(), ctx.Err(), err)
			}
			if !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), "ao account login --operation op") || ExitCode(err) != 1 {
				t.Fatalf("missing failure/reconnect instructions or wrong exit: %v", err)
			}
			if tc.code != "" && (!strings.Contains(err.Error(), tc.code) || !strings.Contains(err.Error(), "verify-request")) {
				t.Fatalf("lost API code/request ID: %v", err)
			}
			if tc.calls > 1 && (!strings.Contains(stderr.String(), "Retry later") || !strings.Contains(stderr.String(), "verify-request") || !strings.Contains(stderr.String(), "retrying")) {
				t.Fatalf("retries were silent: %s", stderr.String())
			}
		})
	}
}
