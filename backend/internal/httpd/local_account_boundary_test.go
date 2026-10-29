package httpd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/agent"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestLANProjectRoundTripPreservesPrivateSettingsWithoutExposingThem(t *testing.T) {
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	path := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main", path}, {"-C", path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "--allow-empty", "-m", "fixture"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git fixture: %v %s", err, out)
		}
	}
	config := domain.ProjectConfig{AgentAccounts: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderCodex: "private-reference-canary"}, Worker: domain.RoleOverride{Harness: domain.HarnessCodex, AgentAccountID: "private-reference-canary"}, Orchestrator: domain.RoleOverride{Harness: domain.HarnessClaudeCode, AgentAccountID: "orchestrator-reference-canary"}, Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex, AgentAccountID: "reviewer-reference-canary"}}, Env: map[string]string{"ANTHROPIC_API_KEY": "credential-canary", "CODEX_HOME": "/private/home-canary", "KEEP": "ordinary"}}
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Kind: domain.ProjectKindSingleRepo, Path: path, Config: config, RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	projects := projectsvc.New(st)
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) { (&controllers.ProjectsController{Mgr: projects}).Register(r) })
	auth := &authState{}
	auth.setHash(mobilebridge.HashPassword("fake-password"))
	lan := NewLANManager(router, auth, 0, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	assertSafe := func(raw string) {
		t.Helper()
		for _, canary := range []string{"private-reference-canary", "reviewer-reference-canary", "orchestrator-reference-canary", "credential-canary", "/private/home-canary"} {
			if strings.Contains(raw, canary) {
				t.Errorf("LAN payload leaked %s", canary)
			}
		}
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer fake-password")
		rec := httptest.NewRecorder()
		lan.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
		assertSafe(rec.Body.String())
		return rec
	}
	get := request(http.MethodGet, "/api/v1/projects/p", "")
	var response struct {
		Project struct {
			Config domain.ProjectConfig `json:"config"`
		} `json:"project"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Project.Config.Env["KEEP"] != "ordinary" {
		t.Fatal("GET lost ordinary project env")
	}
	body, err := json.Marshal(projectsvc.UpdateSettingsInput{DisplayName: "Renamed", Config: response.Project.Config})
	if err != nil {
		t.Fatal(err)
	}
	request(http.MethodPut, "/api/v1/projects/p", string(body))
	request(http.MethodGet, "/api/v1/projects", "")
	local, found, err := st.GetProject(ctx, "p")
	if err != nil || !found {
		t.Fatal(err)
	}
	if local.Config.Env["ANTHROPIC_API_KEY"] != "credential-canary" || local.Config.Worker.AgentAccountID != "private-reference-canary" || local.Config.Orchestrator.AgentAccountID != "orchestrator-reference-canary" || local.Config.Reviewers[0].AgentAccountID != "reviewer-reference-canary" {
		t.Fatal("remote round trip cleared local secrets/references")
	}
	export, err := json.Marshal(local.Config.WithoutLocalAccounts())
	if err != nil {
		t.Fatal(err)
	}
	assertSafe(string(export))
	if _, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex}); err != nil {
		t.Fatal(err)
	}
	events, err := st.EventsAfter(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("missing real CDC events")
	}
	rec := httptest.NewRecorder()
	var sent int64
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		assertSafe(string(raw))
		if err := writeSSEEvent(rec, rec, event, &sent); err != nil {
			t.Fatal(err)
		}
	}
	assertSafe(rec.Body.String())
}

func TestAccountCleanupReportsBlockingReviewerWithoutDeletingProfile(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "codex", "saved")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAgentAccount(ctx, domain.AgentAccount{ID: "saved", Provider: domain.AgentAccountProviderCodex, Home: home, ProfileKind: domain.AgentAccountProfileContained, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	svc := agentaccounts.New(db, []ports.AgentAccountProviderCLI{remoteCodexProvider{}}, root, nil, nil)
	svc.SetAgentStopper(func(context.Context, string) error {
		return &ports.AgentAccountReviewerInUseError{ReviewID: "review-123", SessionID: "worker-1", Harness: domain.ReviewerCodex}
	})
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) { (&controllers.AgentAccountsController{Svc: svc}).Register(r) })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/agents/accounts/saved", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("cleanup busy response: %d %s", rec.Code, rec.Body)
	}
	var response struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != "AGENT_ACCOUNT_IN_USE" || response.Details["reviewId"] != "review-123" || response.Details["reviewerSessionId"] != "worker-1" || response.Details["reviewerHarness"] != "codex" || response.Details["operationId"] == "" {
		t.Fatalf("busy reference missing: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), home) {
		t.Fatal("API exposed profile path")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal("busy profile was deleted")
	}
}

type remoteAccountStore struct{ ports.AgentAccountStore }

func (remoteAccountStore) GetAgentAccountDefault(context.Context, domain.AgentAccountProvider) (string, bool, error) {
	return "private-A", true, nil
}
func (remoteAccountStore) ListAgentAccounts(context.Context) ([]domain.AgentAccount, error) {
	return []domain.AgentAccount{{ID: "private-A", Provider: domain.AgentAccountProviderCodex, Home: "/fake/private/profile"}}, nil
}

type remoteCodexProvider struct{ ports.AgentAccountProviderCLI }

func (remoteCodexProvider) Provider() domain.AgentAccountProvider {
	return domain.AgentAccountProviderCodex
}
func TestLANAccountSelectionCannotReachLocalPrivateProfiles(t *testing.T) {
	accounts := agentaccounts.New(remoteAccountStore{}, []ports.AgentAccountProviderCLI{remoteCodexProvider{}}, filepath.Join(t.TempDir(), "accounts"), nil, nil)
	catalog := agentsvc.NewWithAgents(nil)
	catalog.SetAccountExecution(accounts, nil)
	router := chi.NewRouter()
	router.Route("/api/v1", func(r chi.Router) { (&controllers.AgentsController{Catalog: catalog}).Register(r) })
	state := &authState{}
	state.setHash(mobilebridge.HashPassword("fake-password"))
	lan := NewLANManager(router, state, 0, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	for _, q := range []string{"", "?agentAccountId=private-A", "?agentAccountId=missing", "?agentAccountId=device-codex"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/codex/models"+q, nil)
		req.Header.Set("Authorization", "Bearer fake-password")
		rec := httptest.NewRecorder()
		lan.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "AGENT_ACCOUNT_LOCAL_ONLY") || strings.Contains(rec.Body.String(), "/fake/private") {
			t.Fatalf("LAN selection: %d %s", rec.Code, rec.Body)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "http://attacker.invalid/api/v1/agents/codex/models?agentAccountId=private-A", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatal("Host header was trusted as LAN provenance")
	}
}
