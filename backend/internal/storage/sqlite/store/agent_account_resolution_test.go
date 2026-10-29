package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestResolvedSessionBindingPreservesSourceAndIdempotence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	worker := seedUsageSession(t, s, domain.HarnessClaudeCode)
	at := time.Now().UTC()
	binding := domain.ResolvedAgentAccount{AccountID: "worker-A", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceRole}
	mustNoError(t, s.BindSessionAgentAccountWithSource(ctx, worker.ID, binding, at))
	repeated := binding
	repeated.Source = domain.AgentAccountSourceExplicit
	mustNoError(t, s.BindSessionAgentAccountWithSource(ctx, worker.ID, repeated, at.Add(time.Second)))
	got, ok, err := s.GetSessionAgentAccountBinding(ctx, worker.ID, binding.Provider)
	mustNoError(t, err)
	if !ok || got != binding {
		t.Fatalf("binding=%+v found=%v", got, ok)
	}
}

func TestReviewerBindingIsIndependentAndFirstWriteWins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	worker := seedUsageSession(t, s, domain.HarnessClaudeCode)
	now := time.Now().UTC()
	review := domain.Review{ID: "reviewer-C", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: domain.ReviewerClaudeCode, CreatedAt: now, UpdatedAt: now}
	mustNoError(t, s.UpsertReview(ctx, review))
	workerAccount := domain.ResolvedAgentAccount{AccountID: "worker-A", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceExplicit}
	reviewerAccount := domain.ResolvedAgentAccount{AccountID: "reviewer-C", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceRole}
	mustNoError(t, s.BindSessionAgentAccountWithSource(ctx, worker.ID, workerAccount, now))
	mustNoError(t, s.BindReviewerAgentAccount(ctx, review.ID, reviewerAccount, now))
	changed := reviewerAccount
	changed.AccountID = "new-default-D"
	mustNoError(t, s.BindReviewerAgentAccount(ctx, review.ID, changed, now.Add(time.Minute)))
	got, found, err := s.GetReviewerAgentAccount(ctx, review.ID, reviewerAccount.Provider)
	mustNoError(t, err)
	if !found || got != reviewerAccount {
		t.Fatalf("reviewer drifted: %+v found=%v", got, found)
	}
	own, _, err := s.GetSessionAgentAccountBinding(ctx, worker.ID, workerAccount.Provider)
	mustNoError(t, err)
	if own != workerAccount {
		t.Fatalf("reviewer overwrote worker: %+v", own)
	}
}

func TestPendingReviewerBindingProtectsProfileUntilArchived(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	worker := seedUsageSession(t, s, domain.HarnessCodex)
	now := time.Now().UTC()
	review := domain.Review{ID: "pending-review", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: domain.ReviewerCodex, ReviewerActivityState: domain.ActivityActive, CreatedAt: now, UpdatedAt: now}
	mustNoError(t, s.UpsertReview(ctx, review))
	mustNoError(t, s.BindReviewerAgentAccount(ctx, review.ID, domain.ResolvedAgentAccount{AccountID: "C", Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceRole}, now))
	used, err := s.HasOpenReviewerAccountUse(ctx, "C")
	mustNoError(t, err)
	if !used {
		t.Fatal("pending reviewer without terminal was not protected")
	}
	review.IsArchived = true
	mustNoError(t, s.UpsertReview(ctx, review))
	used, err = s.HasOpenReviewerAccountUse(ctx, "C")
	mustNoError(t, err)
	if used {
		t.Fatal("archived reviewer permanently blocks profile cleanup")
	}
}

func TestResolvedBindingSerializesThreeChoicesAtTheSameInstant(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	worker := seedUsageSession(t, s, domain.HarnessCodex)
	at := time.Now().UTC()
	for _, id := range []string{"A", "B", "C"} {
		binding := domain.ResolvedAgentAccount{AccountID: id, Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceExplicit}
		mustNoError(t, s.BindSessionAgentAccountWithSource(ctx, worker.ID, binding, at))
		got, found, err := s.GetSessionAgentAccountBinding(ctx, worker.ID, binding.Provider)
		mustNoError(t, err)
		if !found || got.AccountID != id {
			t.Fatalf("same-instant choice %s was not pinned: %+v", id, got)
		}
	}
}
