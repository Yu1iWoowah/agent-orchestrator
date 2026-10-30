package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestProjectAccountMutationPreservesOtherSettingsAndUsesStableReviewer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cfg := domain.ProjectConfig{AgentRules: "keep rules", Env: map[string]string{"ordinary": "keep"}, Worker: domain.RoleOverride{Harness: domain.HarnessCodex, AgentAccountID: "worker-old"}, Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerClaudeCode, AgentAccountID: "claude-old"}, {Harness: domain.ReviewerCodex, AgentAccountID: "codex-old"}}}
	mustNoError(t, s.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: "/fake/project", RegisteredAt: time.Now(), Config: cfg}))
	found, err := s.SetProjectAgentAccount(ctx, "p", "C", domain.AgentAccountProviderCodex, "reviewer", "codex", domain.HarnessClaudeCode)
	mustNoError(t, err)
	if !found {
		t.Fatal("project missing")
	}
	got, _, err := s.GetProject(ctx, "p")
	mustNoError(t, err)
	want := cfg
	want.Reviewers = append([]domain.ReviewerConfig(nil), cfg.Reviewers...)
	want.Reviewers[1].AgentAccountID = "C"
	if !reflect.DeepEqual(got.Config, want) {
		t.Fatalf("config=%+v want=%+v", got.Config, want)
	}
	_, err = s.SetProjectAgentAccount(ctx, "p", "wrong", domain.AgentAccountProviderClaude, "worker", "", domain.HarnessClaudeCode)
	if !errors.Is(err, ports.ErrAgentAccountProviderMismatch) {
		t.Fatalf("mismatch=%v", err)
	}
	_, err = s.SetProjectAgentAccount(ctx, "p", "wrong", domain.AgentAccountProviderCodex, "reviewer", "0", domain.HarnessClaudeCode)
	if !errors.Is(err, ports.ErrAgentAccountScopeInvalid) {
		t.Fatalf("array position accepted: %v", err)
	}
}
