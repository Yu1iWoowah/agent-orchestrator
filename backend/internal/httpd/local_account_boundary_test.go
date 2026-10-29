package httpd

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/agent"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
)

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
