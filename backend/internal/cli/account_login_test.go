package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// These are contract examples, not inputs derived from the classifier.
	keys := []string{"token", "access_token", "OAuthToken", "secret", "clientSecret", "password", "passwd", "credential", "providerCredential", "apikey", "api_key", "x-api-key", "customEncryptionKey", "private_key", "authorization", "proxy-authorization", "bearer", "cookie", "set-cookie", "session", "auth"}
	const value = "~PRIVATE_TOKEN_CANARY"
	encodeAll := func(s string) string {
		var out strings.Builder
		for _, c := range []byte(s) {
			_, _ = fmt.Fprintf(&out, "%%%02X", c)
		}
		return out.String()
	}
	mixedCase := func(s string) string {
		var out strings.Builder
		for i, c := range s {
			part := string(c)
			if i%2 == 0 {
				part = strings.ToUpper(part)
			} else {
				part = strings.ToLower(part)
			}
			out.WriteString(part)
		}
		return out.String()
	}
	forms := []struct {
		name string
		line func(string) string
	}{
		{"assignment", func(k string) string { return k + "=" + value + "\r\n" }},
		{"json", func(k string) string { return fmt.Sprintf(`{"%s":"%s"}`, k, value) + "\r\n" }},
		{"nested", func(k string) string { return "headers[" + k + "][0]=" + value + "\r\n" }},
		{"quoted-index", func(k string) string { return "headers[ \"" + k + "\" ][ 0 ]=" + value + "\r\n" }},
		{"dotted", func(k string) string { return "request.headers." + k + ".value=" + value + "\r\n" }},
		{"slash", func(k string) string { return "request/headers/" + k + "/value=" + value + "\r\n" }},
		{"hyphen", func(k string) string { return "request-" + k + "-value=" + value + "\r\n" }},
		{"underscore", func(k string) string { return "request_" + k + "_value=" + value + "\r\n" }},
		{"url-query", func(k string) string { return "https://example.test/?" + k + "=" + value + "\r\n" }},
		{"url-encoded", func(k string) string {
			return "https://example.test/?" + url.QueryEscape("headers[ "+k+" ][0]") + "=" + value + "\r\n"
		}},
		{"fully-encoded", func(k string) string {
			return "https://example.test/?" + encodeAll("request.headers."+k+".value") + "=" + value + "\r\n"
		}},
		{"ambiguous-encoding", func(k string) string {
			return "https://example.test/?%ZZ" + encodeAll("headers["+k+"]") + "=" + value + "\r\n"
		}},
		{"nested-encoding", func(k string) string {
			return "https://example.test/?" + url.QueryEscape(encodeAll("headers["+k+"]")) + "=" + value + "\r\n"
		}},
		{"header", func(k string) string { return k + ": " + value + "\r\n" }},
		{"header-bearer", func(k string) string { return k + ": Bearer " + value + "\r\n" }},
		{"mixed-case", func(k string) string { return mixedCase("headers["+k+"][0]") + "=" + value + "\r\n" }},
		{"ansi", func(k string) string { return k[:len(k)/2] + "\x1b[32m" + k[len(k)/2:] + "\x1b[0m=" + value + "\r\n" }},
		{"osc", func(k string) string {
			return "\x1b]8;;https://example.test/?" + url.QueryEscape(k) + "=" + value + "\x1b\\Sign in\x1b]8;;\x1b\\\r\n"
		}},
	}
	for _, key := range keys {
		for _, form := range forms {
			t.Run(key+"/"+form.name, func(t *testing.T) {
				line := form.line(key)
				var bytesOut bytes.Buffer
				var bytesOutput accountLoginOutput
				for i := range len(line) {
					bytesOutput.write(&bytesOut, []byte(line[i:i+1]))
					if strings.Contains(bytesOut.String(), "~") {
						t.Fatalf("value's first byte escaped at byte %d", i)
					}
				}
				for split := 0; split <= len(line); split++ {
					var out bytes.Buffer
					var output accountLoginOutput
					output.write(&out, []byte(line[:split]))
					if strings.Contains(out.String(), "~") {
						t.Fatalf("value prefix escaped before complete frame, split=%d", split)
					}
					output.write(&out, []byte(line[split:]))
					if strings.Contains(out.String(), "~") {
						t.Fatalf("value escaped, split=%d", split)
					}
				}
			})
		}
	}
	for _, prefix := range []string{"Bearer ", "sk-", "eyJ"} {
		t.Run("bare-value/"+strings.TrimSpace(prefix), func(t *testing.T) {
			var out bytes.Buffer
			var output accountLoginOutput
			line := prefix + value + "\r\n"
			for i := range len(line) {
				output.write(&out, []byte(line[i:i+1]))
				if strings.Contains(out.String(), "~") {
					t.Fatalf("bare credential payload escaped at byte %d", i)
				}
			}
			if !strings.Contains(out.String(), "credential output hidden") {
				t.Fatal("bare value detector was lost")
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

func TestLoginWaitingCancelsBeforeStartingAnotherAttempt(t *testing.T) {
	for _, again := range []bool{false, true} {
		t.Run(fmt.Sprintf("again=%t", again), func(t *testing.T) {
			cfg := setConfigEnv(t)
			var starts, verifies, cancels atomic.Int32
			var busy, cancelled atomic.Bool
			action, retry := "login", "ao account add --provider codex"
			args := []string{"account", "add", "--provider", "codex", "--no-attach"}
			startPath := "/api/v1/agents/accounts/login"
			if again {
				action, retry = "login-again", "ao account login account-A"
				args = []string{"account", "login", "account-A", "--no-attach"}
				startPath = "/api/v1/agents/accounts/account-A/login"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/telemetry/"):
					w.WriteHeader(http.StatusNoContent)
				case r.URL.Path == startPath:
					if busy.Swap(true) {
						w.WriteHeader(http.StatusConflict)
						_, _ = io.WriteString(w, `{"code":"AGENT_ACCOUNT_OPERATION_BUSY","message":"Busy"}`)
						return
					}
					n := starts.Add(1)
					_, _ = fmt.Fprintf(w, `{"operationId":"op-%d","provider":"codex","shellTerminal":{"handleId":"native-login"}}`, n)
				case strings.HasPrefix(r.URL.Path, "/api/v1/agents/accounts/operations/"):
					phase, outcome := "pending", ""
					if starts.Load() == 1 && cancelled.Load() {
						phase, outcome = "complete", "cancelled"
					}
					if starts.Load() == 2 && verifies.Load() == 2 {
						phase, outcome = "complete", "complete"
					}
					_, _ = fmt.Fprintf(w, `{"operationId":"op-%d","accountId":"account-A","provider":"codex","action":%q,"phase":%q,"outcome":%q,"resultAccountId":"account-A","terminalHandleId":"native-login"}`, starts.Load(), action, phase, outcome)
				case r.URL.Path == "/mux":
					serveExitedAccountLogin(t, w, r)
				case strings.HasSuffix(r.URL.Path, "/verify"):
					if verifies.Add(1) == 1 {
						_, _ = io.WriteString(w, `{"status":"waiting"}`)
					} else {
						busy.Store(false)
						_, _ = io.WriteString(w, `{"status":"complete"}`)
					}
				case strings.HasSuffix(r.URL.Path, "/cancel"):
					cancels.Add(1)
					cancelled.Store(true)
					busy.Store(false)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			deps := Deps{ProcessAlive: func(int) bool { return true }}
			_, stderr, err := executeCLI(t, deps, args...)
			if err == nil || ExitCode(err) != 1 || !strings.Contains(err.Error(), "not signed in") || !strings.Contains(err.Error(), retry) || !strings.Contains(stderr, "cleanup confirmed") || cancels.Load() != 1 || busy.Load() {
				t.Fatalf("waiting did not cancel and offer a working retry: err=%v stderr=%s cancels=%d busy=%t", err, stderr, cancels.Load(), busy.Load())
			}
			if strings.Contains(err.Error(), "Settings") || strings.Contains(err.Error(), "--operation") {
				t.Fatal("waiting suggested an unusable recovery route")
			}
			out, _, err := executeCLI(t, deps, args...)
			if err != nil || starts.Load() != 2 || !strings.Contains(out, "account-A saved") {
				t.Fatalf("message's fresh attempt remained busy: %v %s", err, out)
			}
		})
	}
}

func TestLoginWaitingHonorsCleanupFailureAndAcceptedRace(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint("accepted=", accepted), func(t *testing.T) {
			cfg := setConfigEnv(t)
			var cancelled atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/telemetry/"):
					w.WriteHeader(http.StatusNoContent)
				case r.URL.Path == "/mux":
					serveExitedAccountLogin(t, w, r)
				case r.URL.Path == "/api/v1/agents/accounts/operations/op":
					phase, outcome := "pending", ""
					if accepted && cancelled.Load() {
						phase, outcome = "complete", "complete"
					}
					_, _ = fmt.Fprintf(w, `{"operationId":"op","accountId":"A","provider":"codex","action":"login-again","phase":%q,"outcome":%q,"resultAccountId":"A","terminalHandleId":"native-login"}`, phase, outcome)
				case strings.HasSuffix(r.URL.Path, "/verify"):
					_, _ = io.WriteString(w, `{"status":"waiting"}`)
				case strings.HasSuffix(r.URL.Path, "/cancel"):
					cancelled.Store(true)
					if accepted {
						w.WriteHeader(http.StatusNoContent)
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = io.WriteString(w, `{"code":"AGENT_ACCOUNT_CLEANUP_UNCONFIRMED","message":"Cleanup is unconfirmed","requestId":"cleanup-request"}`)
					}
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "account", "login", "--operation", "op", "--no-attach")
			if !cancelled.Load() {
				t.Fatal("waiting left a pending operation")
			}
			if accepted {
				if err != nil || !strings.Contains(out, "account A saved") {
					t.Fatalf("accepted race lost: %v %s", err, out)
				}
			} else if err == nil || !strings.Contains(err.Error(), "cleanup-request") || !strings.Contains(err.Error(), "--operation op") || strings.Contains(stderr, "cleanup confirmed") {
				t.Fatalf("unconfirmed cleanup was misreported: %v %s", err, stderr)
			}
		})
	}
}

func TestLoginLegacyConflictDoesNotClaimOperationRetained(t *testing.T) {
	cfg := setConfigEnv(t)
	var verified atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/telemetry/"):
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/mux":
			serveExitedAccountLogin(t, w, r)
		case strings.HasSuffix(r.URL.Path, "/verify"):
			verified.Store(true)
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"code":"AGENT_ACCOUNT_LEGACY_IDENTITY_CONFLICT","message":"A legacy login already exists","requestId":"legacy-request"}`)
		case r.URL.Path == "/api/v1/agents/accounts/operations/op":
			phase, outcome := "pending", ""
			if verified.Load() {
				phase, outcome = "complete", "legacy-conflict"
			}
			_, _ = fmt.Fprintf(w, `{"operationId":"op","accountId":"A","provider":"codex","action":"login","phase":%q,"outcome":%q,"terminalHandleId":"native-login"}`, phase, outcome)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "account", "login", "--operation", "op", "--no-attach")
	if err == nil || !strings.Contains(err.Error(), "legacy-request") || !strings.Contains(err.Error(), "Settings") || !strings.Contains(err.Error(), "ao account add --provider codex") || strings.Contains(err.Error(), "operation retained") || strings.Contains(err.Error(), "--operation") {
		t.Fatalf("completed legacy conflict had misleading instructions: %v", err)
	}
}
