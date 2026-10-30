package agentaccounts

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestProjectAccountMutationIsLocalAndPreservesConfiguredRoles(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := context.Background()
	root := t.TempDir()
	p := &fakeClaude{signedInAs: map[string]string{}}
	s := New(st, []ports.AgentAccountProviderCLI{p}, root, nil, nil)
	a := domain.AgentAccount{ID: "A", Provider: domain.AgentAccountProviderClaude, Home: filepath.Join(root, "A"), Email: "fake@example.test", CreatedAt: time.Now()}
	if err := st.CreateAgentAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	cfg := domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessClaudeCode}, Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerClaudeCode}}}
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: "/fake/project", Config: cfg, RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectAccount(ports.WithRemoteAccountRequest(ctx), "p", "A", "", ""); !errors.Is(err, ports.ErrAgentAccountLocalOnly) {
		t.Fatalf("remote=%v", err)
	}
	for _, role := range []string{"", "worker", "reviewer"} {
		reviewer := ""
		if role == "reviewer" {
			reviewer = "claude-code"
		}
		if err := s.SetProjectAccount(ctx, "p", "A", role, reviewer); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := st.GetProject(ctx, "p")
	if err != nil || got.Config.AgentAccounts[domain.AgentAccountProviderClaude] != "A" || got.Config.Worker.AgentAccountID != "A" || got.Config.Reviewers[0].AgentAccountID != "A" {
		t.Fatalf("config=%+v err=%v", got.Config, err)
	}
}
