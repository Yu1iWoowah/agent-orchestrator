package project

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestRemoteConfigMutationCannotSelectOrClearLocalAccounts(t *testing.T) {
	current := domain.ProjectConfig{AgentAccounts: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderCodex: "project-A"}, Worker: domain.RoleOverride{AgentAccountID: "worker-A"}, Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex, AgentAccountID: "review-C"}}, Env: map[string]string{"CODEX_HOME": "/fake/private", "OPENAI_API_KEY": "fake-credential-marker"}}
	ctx := ports.WithRemoteAccountRequest(context.Background())
	for _, proposed := range []domain.ProjectConfig{{Worker: domain.RoleOverride{AgentAccountID: "other"}}, {Env: map[string]string{"OPENAI_API_KEY": "marker"}}, {Env: map[string]string{"CODEX_HOME": "/fake/device"}}} {
		if _, err := accountConfigForMutation(ctx, current, proposed); err == nil {
			t.Fatal("remote account mutation accepted")
		}
	}
	projected := current.WithoutLocalAccounts()
	projected.AgentRules = "remote ordinary edit"
	got, err := accountConfigForMutation(ctx, current, projected)
	if err != nil || got.Worker.AgentAccountID != "worker-A" || got.AgentAccounts[domain.AgentAccountProviderCodex] != "project-A" || got.Reviewers[0].AgentAccountID != "review-C" || got.Env["CODEX_HOME"] != "/fake/private" || got.AgentRules != "remote ordinary edit" {
		t.Fatalf("remote edit lost local references: %+v %v", got, err)
	}
}
