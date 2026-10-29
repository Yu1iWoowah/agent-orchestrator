package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAccountUsageOwnersIncludeHistoryReviewerAndUncommittedTarget(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	worker := seedUsageSession(t, s, domain.HarnessClaudeCode)
	_, err := s.RenameSession(ctx, worker.ID, "Fake worker", time.Now().UTC())
	mustNoError(t, err)
	now := time.Now().UTC()
	mustNoError(t, s.BindSessionAgentAccount(ctx, worker.ID, domain.AgentAccountProviderClaude, "old-A", now.Add(-time.Hour)))
	mustNoError(t, s.BindSessionAgentAccount(ctx, worker.ID, domain.AgentAccountProviderClaude, "current-B", now))
	review := domain.Review{ID: "review-C", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: domain.ReviewerClaudeCode, PRURL: "https://github.com/fake/repo/pull/1", ReviewerActivityState: domain.ActivityIdle, CreatedAt: now, UpdatedAt: now}
	mustNoError(t, s.UpsertReview(ctx, review))
	mustNoError(t, s.BindReviewerAgentAccount(ctx, review.ID, domain.ResolvedAgentAccount{AccountID: "target-C", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceExplicit}, now))
	_, _, err = s.CreateSessionInterfaceTransition(ctx, domain.SessionInterfaceTransition{ID: "move-C", SessionID: worker.ID, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, Policy: domain.SessionInterfaceTransitionDrain, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict, Phase: domain.SessionInterfaceTransitionTargetStarting, SourceAccountID: "current-B", TargetAccountID: "target-C", CreatedAt: now, UpdatedAt: now})
	mustNoError(t, err)
	uses, ok := any(s).(ports.AgentAccountUsageStore)
	if !ok {
		t.Fatal("account usage owners are unavailable")
	}
	old, err := uses.ListAgentAccountUsageOwners(ctx, "old-A")
	mustNoError(t, err)
	if len(old) != 0 {
		t.Fatalf("obsolete binding blocked cleanup: %+v", old)
	}
	owners, err := uses.ListAgentAccountUsageOwners(ctx, "target-C")
	mustNoError(t, err)
	if len(owners) != 2 {
		t.Fatalf("missing reviewer or uncommitted target: %+v", owners)
	}
	for _, owner := range owners {
		if !owner.Blocking || owner.SessionID != worker.ID || owner.SessionName != "Fake worker" {
			t.Fatalf("owner metadata: %+v", owner)
		}
		if owner.Kind == "reviewer" && (owner.ReviewID != review.ID || owner.PRURL != review.PRURL) {
			t.Fatalf("reviewer link unavailable: %+v", owner)
		}
	}
}
