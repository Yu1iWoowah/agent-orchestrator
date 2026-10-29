package agentaccounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type reviewerMemoryStore struct {
	memStore
	reviews map[string]domain.ResolvedAgentAccount
}

func (s *reviewerMemoryStore) BindReviewerAgentAccount(_ context.Context, id string, binding domain.ResolvedAgentAccount, _ time.Time) error {
	key := id + ":" + string(binding.Provider)
	if _, ok := s.reviews[key]; !ok {
		s.reviews[key] = binding
	}
	return nil
}
func (s *reviewerMemoryStore) GetReviewerAgentAccount(_ context.Context, id string, p domain.AgentAccountProvider) (domain.ResolvedAgentAccount, bool, error) {
	b, ok := s.reviews[id+":"+string(p)]
	return b, ok, nil
}
func TestReviewerResolutionStaysIndependentAcrossDefaultsAndRestore(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "accounts")
	store := &reviewerMemoryStore{memStore: memStore{defaults: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderClaude: "A"}, bindings: map[domain.SessionID]string{}}, reviews: map[string]domain.ResolvedAgentAccount{}}
	cli := &fakeClaude{signedInAs: map[string]string{}}
	for _, id := range []string{"A", "B", "C", "D"} {
		home := filepath.Join(root, "claude", id)
		if err := os.MkdirAll(home, 0700); err != nil {
			t.Fatal(err)
		}
		email := id + "@example.test"
		store.accounts = append(store.accounts, domain.AgentAccount{ID: id, Provider: domain.AgentAccountProviderClaude, Home: home, Email: email})
		cli.signedInAs[home] = email
	}
	svc := New(store, []ports.AgentAccountProviderCLI{cli}, root, nil, nil)
	cfg := domain.ProjectConfig{Worker: domain.RoleOverride{AgentAccountID: "A"}, Orchestrator: domain.RoleOverride{AgentAccountID: "B"}, Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerClaudeCode, AgentAccountID: "C"}}}
	request := ports.AgentAccountResolutionRequest{Harness: domain.HarnessClaudeCode, Role: domain.AgentAccountRoleReviewer, ProjectConfig: cfg}
	got, err := svc.ResolveReviewerAccount(ctx, "review", request)
	if err != nil || got.AccountID != "C" || got.Source != domain.AgentAccountSourceRole {
		t.Fatalf("review resolution=%+v err=%v", got, err)
	}
	store.defaults[domain.AgentAccountProviderClaude] = "D"
	cfg.Reviewers[0].AgentAccountID = "D"
	request.ProjectConfig = cfg
	restored := New(store, []ports.AgentAccountProviderCLI{cli}, root, nil, nil)
	got, err = restored.ResolveReviewerAccount(ctx, "review", request)
	if err != nil || got.AccountID != "C" {
		t.Fatalf("reviewer drifted on restore: %+v %v", got, err)
	}
	env, err := restored.ReviewerLaunchEnv(ctx, "review", domain.HarnessClaudeCode)
	if err != nil || env["CLAUDE_CONFIG_DIR"] != filepath.Join(root, "claude", "C") {
		t.Fatalf("reviewer env used worker/default: %+v %v", env, err)
	}
	request.AccountID = "D"
	if _, err := restored.ResolveReviewerAccount(ctx, "review", request); !errors.Is(err, ports.ErrAgentAccountChanged) {
		t.Fatalf("existing reviewer switched identity: %v", err)
	}
	if len(store.bindings) != 0 {
		t.Fatal("reviewer touched worker session bindings")
	}
}
