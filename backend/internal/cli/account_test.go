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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type rhCatalogProvider struct {
	provider domain.AgentAccountProvider
	mu       sync.Mutex
	calls    map[string][]string
}

func (p *rhCatalogProvider) record(home, operation string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[home] = append(p.calls[home], operation)
}
func (p *rhCatalogProvider) Provider() domain.AgentAccountProvider { return p.provider }
func (p *rhCatalogProvider) Identity(_ context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	p.record(home, "identity")
	return domain.AgentAccountIdentityCheck{SignedIn: true, Email: "fixture@example.invalid"}, nil
}
func (p *rhCatalogProvider) Limits(_ context.Context, home string) (domain.AgentAccountLimits, error) {
	p.record(home, "limits")
	return domain.AgentAccountLimits{}, nil
}
func (p *rhCatalogProvider) Prepare(home string) error { p.record(home, "prepare"); return nil }
func (*rhCatalogProvider) LoginArgv(context.Context) ([]string, error) {
	return nil, errors.New("login forbidden in RH catalog test")
}
func (*rhCatalogProvider) Logout(context.Context, string) error {
	return errors.New("logout forbidden in RH catalog test")
}
func (*rhCatalogProvider) LaunchEnv(string) map[string]string { return nil }

func TestAccountRefreshIsGlobalBeforeCLIFilter(t *testing.T) {
	cfg := setConfigEnv(t)
	store := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	providers := []*rhCatalogProvider{
		{provider: domain.AgentAccountProviderClaude, calls: map[string][]string{}},
		{provider: domain.AgentAccountProviderCodex, calls: map[string][]string{}},
	}
	var adapters []ports.AgentAccountProviderCLI
	for _, provider := range providers {
		adapters = append(adapters, provider)
		for _, suffix := range []string{"one", "two"} {
			id := string(provider.provider) + "-" + suffix
			home := filepath.Join(root, string(provider.provider), id)
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := store.CreateAgentAccount(context.Background(), domain.AgentAccount{
				ID: id, Provider: provider.provider, Home: home, CreatedAt: time.Now(),
				ProfileKind: domain.AgentAccountProfileContained,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	service := agentaccounts.New(store, adapters, root, nil, nil)
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) { (&controllers.AgentAccountsController{Svc: service}).Register(r) })
	server := newFixtureServer(t, router)
	defer server.Close()
	writeRunFileFor(t, cfg, server)
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "account", "ls", "--provider", "codex", "--refresh", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Accounts []struct {
			Provider string `json:"provider"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Accounts) != 3 {
		t.Fatalf("filtered accounts=%d, want device plus two", len(response.Accounts))
	}
	for _, account := range response.Accounts {
		if account.Provider != "codex" {
			t.Fatalf("CLI filter leaked provider %s", account.Provider)
		}
	}
	for _, provider := range providers {
		provider.mu.Lock()
		for _, suffix := range []string{"one", "two"} {
			home := filepath.Join(root, string(provider.provider), string(provider.provider)+"-"+suffix)
			if got := strings.Join(provider.calls[home], ","); got != "prepare,identity,limits" {
				t.Errorf("provider=%s profile=%s operations=%q", provider.provider, suffix, got)
			}
		}
		provider.mu.Unlock()
	}
}

func TestAccountUsageErrorsDoNotCallDaemon(t *testing.T) {
	for _, args := range [][]string{{"ls", "--provider", ""}, {"use", "A", "--default", "--project", ""}, {"use", "A", "--default", "--session", " "}, {"use", "A"}, {"use", "A", "--default", "--session", "s"}, {"use", "A", "--default", "--role", "worker"}, {"use", "A", "--project", "p", "--role", "reviewer"}, {"use", "A", "--project", "p"}, {"use", "A", "--project", "p", "--wait"}, {"add"}, {"add", "--provider", "other"}, {"login"}, {"rm"}} {
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
			srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			_, _ = io.WriteString(w, `{"status":"waiting"}`)
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
	root := newTestRootCommand(t, Deps{Out: &out, Err: &stderr, HTTPClient: apiFixtureClient(t, nil), ProcessAlive: func(int) bool { return true }})
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
			srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	root := newTestRootCommand(t, Deps{Err: &stderr, Out: io.Discard, HTTPClient: apiFixtureClient(t, nil), ProcessAlive: func(int) bool { return true }})
	root.SetArgs([]string{"account", "add", "--provider", "codex", "--no-attach"})
	err := root.ExecuteContext(ctx)
	if !cancelled || !errors.Is(err, context.Canceled) || !strings.Contains(stderr.String(), "cleanup confirmed") {
		t.Fatalf("cancel=%t err=%v stderr=%s", cancelled, err, stderr.String())
	}
}

func TestAccountRemoveShowsOnlyBlockingOwnersWithoutStoppingThem(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

type accountServer struct {
	mu       sync.Mutex
	requests []string
	switches []switchSessionAccountRequest
	polls    int
	final    string
}

func (a *accountServer) log(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r.Method+" "+r.URL.RequestURI())
}

func newAccountServer(t *testing.T, final string) (*httptest.Server, *accountServer) {
	t.Helper()
	state := &accountServer{final: final}
	accounts := `{"accounts":[` +
		`{"id":"device-claude","provider":"claude","device":true,"email":"me@example.com","plan":"max","default":true,"signedIn":true,"loggedIn":true},` +
		`{"id":"work","provider":"claude","device":false,"email":"work@example.com","plan":"team","default":false,"signedIn":true,"loggedIn":true,"orgId":"org-team-1","orgName":"Team"},` +
		`{"id":"device-codex","provider":"codex","device":true,"default":true,"signedIn":false,"loggedIn":false}]}`
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		state.log(r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/accounts":
			_, _ = io.WriteString(w, accounts)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/accounts/work/default":
			_, _ = io.WriteString(w, accounts)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/demo-1":
			_, _ = io.WriteString(w, `{"session":`+sessionJSON("demo-1", "demo", "worker", "idle", false)+`}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/ghost":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not_found","code":"SESSION_NOT_FOUND","message":"Unknown session"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/accounts/sessions/demo-1":
			_, _ = io.WriteString(w, `{"claude":"work","codex":"device-codex"}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/agents/accounts/sessions/demo-1":
			var req switchSessionAccountRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			state.mu.Lock()
			state.switches = append(state.switches, req)
			state.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"ok":true,"sessionId":"demo-1","transition":{"id":"t-1","phase":"requested","targetAccountId":`+jsonQuote(req.AccountID)+`}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/demo-1/interface-transition":
			state.mu.Lock()
			state.polls++
			id, phase := "t-1", "draining"
			if state.polls > 1 {
				phase = state.final
			}
			if phase == "superseded" {
				id, phase = "t-2", "draining"
			}
			state.mu.Unlock()
			_, _ = io.WriteString(w, `{"supported":true,"targetMode":"chat","transition":{"id":"`+id+`","phase":"`+phase+`","errorDetail":"the account is signed out"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, state
}

func runAccountCLI(t *testing.T, final string, args ...string) (string, *accountServer, error) {
	t.Helper()
	cfg := setConfigEnv(t)
	srv, state := newAccountServer(t, final)
	writeRunFileFor(t, cfg, srv)
	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
	if err != nil {
		t.Logf("stderr: %s", errOut)
	}
	return out, state, err
}

func TestAccountListFiltersByAgent(t *testing.T) {
	out, _, err := runAccountCLI(t, "completed", "account", "ls", "--agent", "claude-code")
	if err != nil {
		t.Fatalf("account ls: %v", err)
	}
	for _, want := range []string{"device-claude", "me@example.com [this computer's login]", "signed in, default", "work@example.com"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "device-codex") {
		t.Fatalf("codex account listed under --agent claude-code:\n%s", out)
	}
}

func TestAccountListTellsOrganizationsApart(t *testing.T) {
	accounts := []accountInfo{
		{ID: "acme-1", Provider: "claude", Email: "dev@example.com", Plan: "team", SignedIn: true, OrgID: "aaaaaaaa-1111", OrgName: "Acme"},
		{ID: "acme-2", Provider: "claude", Email: "Dev@example.com", Plan: "team", SignedIn: true, OrgID: "bbbbbbbb-2222", OrgName: "Acme"},
		{ID: "personal", Provider: "claude", Email: "dev@example.com", Plan: "max", SignedIn: true, OrgID: "cccccccc-3333", OrgName: "Personal"},
		{ID: "solo", Provider: "codex", SignedIn: true},
	}
	var out strings.Builder
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := writeAccountList(cmd, accounts); err != nil {
		t.Fatal(err)
	}
	orgs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n")[1:] {
		fields := strings.Split(line, "  ")
		var cells []string
		for _, field := range fields {
			if field = strings.TrimSpace(field); field != "" {
				cells = append(cells, field)
			}
		}
		orgs[cells[0]] = cells[3]
	}
	want := map[string]string{"acme-1": "Acme (aaaaaaaa)", "acme-2": "Acme (bbbbbbbb)", "personal": "Personal", "solo": "-"}
	if !reflect.DeepEqual(orgs, want) {
		t.Fatalf("ORG column = %#v, want %#v\n%s", orgs, want, out.String())
	}
}

func TestAccountListJSONKeepsOrganization(t *testing.T) {
	out, _, err := runAccountCLI(t, "completed", "account", "ls", "--json")
	if err != nil {
		t.Fatalf("account ls --json: %v", err)
	}
	var res accountList
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	for _, account := range res.Accounts {
		if account.ID == "work" && (account.OrgID != "org-team-1" || account.OrgName != "Team") {
			t.Fatalf("work account lost its organization: %#v", account)
		}
	}
}

func TestAccountListRejectsUnknownAgent(t *testing.T) {
	if _, _, err := runAccountCLI(t, "completed", "account", "ls", "--agent", "aider"); err == nil {
		t.Fatal("account ls --agent aider succeeded")
	}
}

func TestAccountDefault(t *testing.T) {
	out, state, err := runAccountCLI(t, "completed", "account", "default", "work")
	if err != nil {
		t.Fatalf("account default: %v", err)
	}
	if !strings.Contains(out, "work (work@example.com) is now the default for new claude-code sessions") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if want := []string{"POST /api/v1/agents/accounts/work/default"}; !reflect.DeepEqual(state.requests, want) {
		t.Fatalf("requests = %#v, want %#v", state.requests, want)
	}
}

func TestAccountShowDefaultsToThisSession(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "demo-1")
	out, _, err := runAccountCLI(t, "completed", "account", "show")
	if err != nil {
		t.Fatalf("account show: %v", err)
	}
	if out != "claude-code: work\ncodex: device-codex\n" {
		t.Fatalf("unexpected output:\n%q", out)
	}
}

func TestAccountShowRefusesUnknownSession(t *testing.T) {
	out, state, err := runAccountCLI(t, "completed", "account", "show", "ghost", "--json")
	if err == nil || !strings.Contains(err.Error(), "Unknown session") {
		t.Fatalf("account show ghost: err = %v, out = %q", err, out)
	}
	if out != "" {
		t.Fatalf("printed accounts for an unknown session: %q", out)
	}
	if want := []string{"GET /api/v1/sessions/ghost"}; !reflect.DeepEqual(state.requests, want) {
		t.Fatalf("requests = %#v, want %#v", state.requests, want)
	}
}

func TestAccountSwitchStartsWithoutWaiting(t *testing.T) {
	out, state, err := runAccountCLI(t, "completed", "account", "switch", "demo-1", "work", "--stop-turn")
	if err != nil {
		t.Fatalf("account switch: %v", err)
	}
	if !strings.Contains(out, "moving demo-1 to work (transition t-1, requested)") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if want := []switchSessionAccountRequest{{AccountID: "work", StopTurn: true}}; !reflect.DeepEqual(state.switches, want) {
		t.Fatalf("switch bodies = %#v, want %#v", state.switches, want)
	}
	if state.polls != 0 {
		t.Fatalf("polled %d times without --wait", state.polls)
	}
}

func TestAccountSwitchWaits(t *testing.T) {
	old := accountSwitchPoll
	accountSwitchPoll = time.Millisecond
	t.Cleanup(func() { accountSwitchPoll = old })

	out, state, err := runAccountCLI(t, "completed", "account", "switch", "demo-1", "work", "--wait")
	if err != nil {
		t.Fatalf("account switch --wait: %v", err)
	}
	if !strings.Contains(out, "moved demo-1 to work") || state.polls != 2 {
		t.Fatalf("output %q after %d polls", out, state.polls)
	}

	_, _, err = runAccountCLI(t, "failed", "account", "switch", "demo-1", "work", "--wait")
	if err == nil || !strings.Contains(err.Error(), "ended failed: the account is signed out") {
		t.Fatalf("failed move error = %v", err)
	}
}

func TestAccountSwitchWaitStopsWhenSuperseded(t *testing.T) {
	old := accountSwitchPoll
	accountSwitchPoll = time.Millisecond
	t.Cleanup(func() { accountSwitchPoll = old })

	_, state, err := runAccountCLI(t, "superseded", "account", "switch", "demo-1", "work", "--wait", "--timeout", "5s")
	if err == nil || !strings.Contains(err.Error(), "later change (transition t-2) replaced it") {
		t.Fatalf("superseded move error = %v", err)
	}
	if state.polls != 2 {
		t.Fatalf("polled %d times, want 2", state.polls)
	}
}

func TestAccountLoginShowsBlockingOwnersFromRefusal(t *testing.T) {
	cfg := setConfigEnv(t)
	srv := newFixtureServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/telemetry/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/agents/accounts/A/login" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"AGENT_ACCOUNT_IN_USE","message":"Account is held.","requestId":"login-request","details":{"usageOwners":[{"kind":"transition","sessionId":"active","transitionId":"move-1","blocking":true},{"kind":"session","sessionId":"old","blocking":false}]}}`)
	}))
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	_, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "account", "login", "A", "--detach")
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "login-request") || !strings.Contains(stderr, "active") || !strings.Contains(stderr, "move-1") || strings.Contains(stderr, "old") {
		t.Fatalf("err=%v stderr=%s", err, stderr)
	}
}
