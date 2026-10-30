package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccountUsageErrorsDoNotCallDaemon(t *testing.T) {
	for _, args := range [][]string{{"ls", "--provider", ""}, {"use", "A", "--default", "--project", ""}, {"use", "A", "--default", "--session", " "}, {"use", "A"}, {"use", "A", "--default", "--session", "s"}, {"use", "A", "--default", "--role", "worker"}, {"use", "A", "--project", "p", "--role", "reviewer"}, {"use", "A", "--project", "p", "--wait"}, {"add"}, {"add", "--provider", "other"}, {"login"}, {"rm"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, err := executeCLI(t, Deps{}, append([]string{"account"}, args...)...)
			if ExitCode(err) != 2 {
				t.Fatalf("exit=%d err=%v", ExitCode(err), err)
			}
		})
	}
}

func TestAccountCommandsUseOnlyDaemonRoutes(t *testing.T) {
	for _, tt := range []struct {
		args               []string
		method, path, body string
	}{
		{[]string{"ls", "--provider", "claude", "--refresh", "--json"}, "GET", "/api/v1/agents/accounts?refresh=true", ""},
		{[]string{"use", "A", "--default"}, "POST", "/api/v1/agents/accounts/A/default", "{}"},
		{[]string{"use", "A", "--project", "p", "--role", "reviewer", "--reviewer", "codex"}, "PUT", "/api/v1/agents/accounts/A/projects/p", `{"role":"reviewer","reviewer":"codex"}`},
		{[]string{"use", "A", "--session", "s", "--stop-turn"}, "PUT", "/api/v1/agents/accounts/sessions/s", `{"accountId":"A","stopTurn":true}`},
		{[]string{"logout", "A"}, "POST", "/api/v1/agents/accounts/A/logout", "{}"},
		{[]string{"rm", "A"}, "DELETE", "/api/v1/agents/accounts/A", ""},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			cfg := setConfigEnv(t)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/telemetry/") {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				calls++
				if r.Method != tt.method || r.URL.RequestURI() != tt.path {
					t.Errorf("request=%s %s", r.Method, r.URL.RequestURI())
				}
				b, _ := io.ReadAll(r.Body)
				if tt.body != "" {
					var got, want any
					_ = json.Unmarshal(b, &got)
					_ = json.Unmarshal([]byte(tt.body), &want)
					if fmt.Sprint(got) != fmt.Sprint(want) {
						t.Errorf("body=%s", b)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"accounts":[{"id":"A","provider":"claude","email":"fake@example.test","signedIn":true}],"transition":{"id":"t","phase":"target_starting"}}`)
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }, LookPath: func(string) (string, error) { t.Fatal("provider lookup"); return "", nil }}, append([]string{"account"}, tt.args...)...)
			if err != nil || calls != 1 {
				t.Fatalf("calls=%d err=%v out=%s", calls, err, out)
			}
		})
	}
}

func TestAccountDaemonFailurePreservesRequestID(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/telemetry/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"AGENT_ACCOUNT_PROVIDER_MISMATCH","message":"Choose an account for this provider.","requestId":"fake-request-17"}`)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "account", "use", "A", "--session", "s")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "fake-request-17") || !strings.Contains(err.Error(), "AGENT_ACCOUNT_PROVIDER_MISMATCH") {
		t.Fatalf("error=%v exit=%d", err, ExitCode(err))
	}
}

func TestSpawnExplicitAccountReachesDaemonWithoutGlobalReadiness(t *testing.T) {
	cfg := setConfigEnv(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/telemetry/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/v1/sessions" {
			t.Errorf("unexpected global probe %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["agentAccountId"] != "managed-A" {
			t.Errorf("request=%v", req)
		}
		called = true
		_, _ = io.WriteString(w, `{"session":{"id":"fake-1","status":"idle"}}`)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "spawn", "--standalone", "--agent", "codex", "--name", "Fake worker", "--account", "managed-A")
	if err != nil || !called {
		t.Fatalf("called=%t error=%v", called, err)
	}
}

func TestAccountLoginReconnectDoesNotStartAnotherProfileOrPrintProviderFields(t *testing.T) {
	cfg := setConfigEnv(t)
	verifies := 0
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/telemetry/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/mux":
			serveExitedAccountLogin(t, w, r)
		case "/api/v1/agents/accounts/operations/op-A":
			gets++
			phase := "pending"
			outcome := ""
			if verifies > 0 {
				phase, outcome = "complete", "complete"
			}
			_, _ = fmt.Fprintf(w, `{"operationId":"op-A","action":"login","phase":%q,"outcome":%q,"resultAccountId":"A","terminalHandleId":"shellterm-A","providerOutput":"fake-credential-marker"}`, phase, outcome)
		case "/api/v1/agents/accounts/login/op-A/verify":
			verifies++
			_, _ = io.WriteString(w, `{"status":"complete","account":{"id":"A"}}`)
		default:
			t.Errorf("new profile or unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "account", "login", "--operation", "op-A", "--no-attach", "--json")
	if err != nil || verifies != 1 || gets < 2 {
		t.Fatalf("verify=%d gets=%d err=%v", verifies, gets, err)
	}
	if strings.Contains(out+stderr, "fake-credential-marker") || strings.Contains(out, "terminalHandleId") {
		t.Fatalf("private native output leaked: %s %s", out, stderr)
	}
	if !strings.Contains(out, `"outcome": "complete"`) {
		t.Fatalf("completion=%s", out)
	}
}
func TestAccountLoginCancelUsesFreshContextAndChecksDurableOutcome(t *testing.T) {
	cfg := setConfigEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/telemetry/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.URL.Path {
		case "/mux":
			serveExitedAccountLogin(t, w, r)
		case "/api/v1/agents/accounts/login":
			_, _ = io.WriteString(w, `{"operationId":"op-A","provider":"codex","shellTerminal":{"handleId":"shellterm-A"}}`)
		case "/api/v1/agents/accounts/login/op-A/verify":
			cancel()
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agents/accounts/login/op-A/cancel":
			cancelled = true
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agents/accounts/operations/op-A":
			if cancelled {
				_, _ = io.WriteString(w, `{"operationId":"op-A","phase":"complete","outcome":"cancelled"}`)
			} else {
				_, _ = io.WriteString(w, `{"operationId":"op-A","phase":"pending"}`)
			}
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	var out, stderr bytes.Buffer
	root := NewRootCommand(Deps{Out: &out, Err: &stderr, ProcessAlive: func(int) bool { return true }})
	root.SetArgs([]string{"account", "add", "--provider", "codex", "--no-attach"})
	err := root.ExecuteContext(ctx)
	if !cancelled || !errors.Is(err, context.Canceled) || !strings.Contains(stderr.String(), "cleanup confirmed") {
		t.Fatalf("cancel=%t err=%v stderr=%s", cancelled, err, stderr.String())
	}
}
func TestAccountUseWaitRequiresDurableCompleted(t *testing.T) {
	for _, phase := range []string{"completed", "recovery_required"} {
		t.Run(phase, func(t *testing.T) {
			cfg := setConfigEnv(t)
			gets := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/telemetry/") {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method == http.MethodPut {
					w.WriteHeader(http.StatusAccepted)
					_, _ = io.WriteString(w, `{"transition":{"id":"t1","phase":"target_starting"}}`)
					return
				}
				gets++
				_, _ = fmt.Fprintf(w, `{"transition":{"id":"t1","phase":%q,"errorCode":"TARGET_STOP_UNCONFIRMED"}}`, phase)
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "account", "use", "A", "--session", "s", "--wait")
			if gets != 1 || (phase == "completed") != (err == nil) {
				t.Fatalf("gets=%d err=%v", gets, err)
			}
		})
	}
}

func TestAccountLoginCancelDuringStartRetainsExactOperationReference(t *testing.T) {
	cfg := setConfigEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/telemetry/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.URL.Path {
		case "/api/v1/agents/accounts/login":
			cancel()
			_, _ = io.WriteString(w, `{"operationId":"start-race","provider":"codex"}`)
		case "/api/v1/agents/accounts/login/start-race/cancel":
			cancelled = true
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agents/accounts/operations/start-race":
			_, _ = io.WriteString(w, `{"operationId":"start-race","phase":"complete","outcome":"cancelled"}`)
		default:
			t.Errorf("unexpected request after start cancellation: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	var stderr bytes.Buffer
	root := NewRootCommand(Deps{Err: &stderr, Out: io.Discard, ProcessAlive: func(int) bool { return true }})
	root.SetArgs([]string{"account", "add", "--provider", "codex", "--no-attach"})
	err := root.ExecuteContext(ctx)
	if !cancelled || !errors.Is(err, context.Canceled) || !strings.Contains(stderr.String(), "cleanup confirmed") {
		t.Fatalf("cancel=%t err=%v stderr=%s", cancelled, err, stderr.String())
	}
}

func TestAccountRemoveShowsOnlyBlockingOwnersWithoutStoppingThem(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/telemetry/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/agents/accounts/A":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"code":"AGENT_ACCOUNT_IN_USE","message":"Stop or hand off active owners.","requestId":"owner-request"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/usage-owners"):
			_, _ = io.WriteString(w, `{"owners":[{"kind":"session","sessionId":"active","sessionName":"Fake active worker","blocking":true},{"kind":"session","sessionId":"old","sessionName":"Fake retired history","blocking":false}]}`)
		default:
			t.Errorf("remove attempted side effect: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	_, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "account", "rm", "A")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "owner-request") || !strings.Contains(stderr, "Fake active worker") || strings.Contains(stderr, "Fake retired history") {
		t.Fatalf("err=%v stderr=%s", err, stderr)
	}
}

func TestLoginOutputHidesCredentialsInANSIAndHyperlinkURI(t *testing.T) {
	for name, line := range map[string]string{
		"plain":                "access_token=PRIVATE_TOKEN_CANARY\r\n",
		"edited-prefix":        "sl\bk-PRIVATE_TOKEN_CANARY\r\n",
		"authorization-spaces": "Authorization" + strings.Repeat(" ", 128) + ": PRIVATE_TOKEN_CANARY\r\n",
		"bearer-tab":           "Bearer\tPRIVATE_TOKEN_CANARY\r\n",
		"colour":               "ac\x1b[32mcess_token\x1b[0m=PRIVATE_TOKEN_CANARY\r\n",
		"hyperlink":            "\x1b]8;;https://example.test/?access_token=PRIVATE_TOKEN_CANARY\x1b\\Sign in\x1b]8;;\x1b\\\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			var output accountLoginOutput
			output.write(&out, []byte(line))
			got := out.String()
			if strings.Contains(got, "TOKEN_CANARY") || (name != "hyperlink" && !strings.Contains(got, "credential output hidden")) {
				t.Fatal("credential reached live login output")
			}
		})
	}
	prompt := "Open https://example.test/device and enter ABCD-1234\r\n"
	var out bytes.Buffer
	var output accountLoginOutput
	output.write(&out, []byte(prompt))
	if out.String() != prompt {
		t.Fatal("device authorization prompt was hidden")
	}
}

func TestLoginOutputDropsLongCredentialContinuationAndKeepsNextPrompt(t *testing.T) {
	var out bytes.Buffer
	var output accountLoginOutput
	output.write(&out, []byte("access_token="+strings.Repeat("x", 70*1024)))
	output.write(&out, []byte("PRIVATE_TOKEN_CANARY\r\nOpen device sign-in and enter ABCD-1234\r\n"))
	if strings.Contains(out.String(), "TOKEN_CANARY") || !strings.Contains(out.String(), "credential output hidden") || !strings.Contains(out.String(), "Open device sign-in") {
		t.Fatal("long credential leaked or subsequent prompt was lost")
	}
	output.write(&out, []byte("access_to"))
	output.write(&out, []byte("ken=PRIVATE_TOKEN_CANARY\r\n"))
	if strings.Contains(out.String(), "TOKEN_CANARY") {
		t.Fatal("credential split across frames reached output")
	}
}
