package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type reviewerReconcilerFunc func(context.Context, string) error

func (f reviewerReconcilerFunc) ReconcileAccountReviewers(ctx context.Context, id string) error {
	return f(ctx, id)
}

// Logout reconciles every reviewer holding the account before its reviewer
// check: two reviewers with no process left no longer refuse it. A failed
// reconcile keeps the account in use.
func TestAccountStopperReconcilesReviewersBeforeRefusing(t *testing.T) {
	for _, reconcileErr := range []error{nil, errors.New("review store unavailable")} {
		ctx := context.Background()
		store := sqlitetest.MustOpen(t)
		now := time.Now().UTC()
		if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: "/repo/p", RegisteredAt: now}); err != nil {
			t.Fatal(err)
		}
		var reviews []domain.Review
		for _, id := range []string{"gone-1", "gone-2"} {
			rec, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			review := domain.Review{ID: id, SessionID: rec.ID, ProjectID: "p", Harness: domain.ReviewerCodex, ReviewerActivityState: domain.ActivityActive, CreatedAt: now, UpdatedAt: now}
			if err := store.UpsertReview(ctx, review); err != nil {
				t.Fatal(err)
			}
			if err := store.BindReviewerAgentAccount(ctx, id, domain.ResolvedAgentAccount{AccountID: "acct", Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceExplicit}, now); err != nil {
				t.Fatal(err)
			}
			reviews = append(reviews, review)
		}
		reconcile := reviewerReconcilerFunc(func(ctx context.Context, id string) error {
			if reconcileErr != nil {
				return reconcileErr
			}
			for _, review := range reviews {
				review.ReviewerActivityState = domain.ActivityExited
				if err := store.UpsertReview(ctx, review); err != nil {
					return err
				}
			}
			return nil
		})
		stopped := false
		err := accountAgentStopper(store, reconcile, func(context.Context, string) error { stopped = true; return nil })(ctx, "acct")
		if reconcileErr == nil && (err != nil || !stopped) {
			t.Fatalf("reconciled reviewers still refused logout: %v stopped=%t", err, stopped)
		}
		if reconcileErr != nil && (!errors.Is(err, ports.ErrAgentAccountStorageUnavailable) || stopped) {
			t.Fatalf("failed reconcile = %v stopped=%t, want storage unavailable", err, stopped)
		}
	}
}
