package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLocalAccountReferencesDoNotEnterExportAndMatchStableReviewerHarness(t *testing.T) {
	local := ProjectConfig{Env: map[string]string{"KEEP": "yes", "OPENAI_API_KEY": "fake-credential-marker", "CLAUDE_CONFIG_DIR": "/fake/private-profile"}, AgentAccounts: map[AgentAccountProvider]string{AgentAccountProviderCodex: "project-private"}, Worker: RoleOverride{AgentAccountID: "worker-private", AgentConfig: AgentConfig{Model: "keep-model"}}, Orchestrator: RoleOverride{AgentAccountID: "orchestrator-private"}, Reviewers: []ReviewerConfig{{Harness: ReviewerClaudeCode, AgentAccountID: "claude-private"}, {Harness: ReviewerCodex, AgentAccountID: "codex-private"}}}
	public := local.WithoutLocalAccounts()
	wire, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "private") || strings.Contains(string(wire), "agentAccount") || strings.Contains(string(wire), "fake-credential-marker") || strings.Contains(string(wire), "/fake/") {
		t.Fatalf("local references entered export: %s", wire)
	}
	if !local.HasLocalAccountReferences() || public.HasLocalAccountReferences() || public.Worker.AgentConfig.Model != "keep-model" {
		t.Fatal("projection changed unrelated settings or local original")
	}
	public.Reviewers[0], public.Reviewers[1] = public.Reviewers[1], public.Reviewers[0]
	restored := public.WithLocalAccountReferences(local)
	if restored.Env["OPENAI_API_KEY"] != "fake-credential-marker" || public.Env["KEEP"] != "yes" {
		t.Fatal("private env was lost or unrelated env changed")
	}
	if restored.Reviewers[0].AgentAccountID != "codex-private" || restored.Reviewers[1].AgentAccountID != "claude-private" {
		t.Fatal("reviewer references moved by array index")
	}
	restored.AgentAccounts[AgentAccountProviderCodex] = "changed"
	if local.AgentAccounts[AgentAccountProviderCodex] != "project-private" {
		t.Fatal("projection shares mutable local reference map")
	}
}
