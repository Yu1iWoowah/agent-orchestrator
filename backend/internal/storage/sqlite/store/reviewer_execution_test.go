package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestExplicitReviewerRebuildCreatesNewOwnerAndPreservesPassHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	worker := seedUsageSession(t, s, domain.HarnessCodex)
	now := time.Now().UTC()
	old := domain.Review{ID: "old-execution", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: domain.ReviewerCodex, AgentSessionID: "old-native", CreatedAt: now, UpdatedAt: now}
	mustNoError(t, s.UpsertReview(ctx, old))
	mustNoError(t, s.BindReviewerAgentAccount(ctx, old.ID, domain.ResolvedAgentAccount{AccountID: "A", Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceExplicit}, now))
	store, ok := any(s).(ports.ReviewerExecutionStore)
	if !ok {
		t.Fatal("explicit reviewer rebuild is unavailable")
	}
	target := domain.ResolvedAgentAccount{AccountID: "B", Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceExplicit}
	if _, created, err := store.CreateReviewerAccountExecution(ctx, old.ID, "refused-live", target, now); err != nil || created {
		t.Fatalf("active reviewer was rebuilt: created=%t %v", created, err)
	}
	run := domain.ReviewRun{ID: "old-pass", ReviewID: old.ID, SessionID: worker.ID, Harness: old.Harness, PRURL: "https://github.com/fake/repo/pull/1", TargetSHA: "old-head", Status: domain.ReviewRunRunning, CreatedAt: now}
	mustNoError(t, s.InsertReviewRun(ctx, run))
	mustNoError(t, s.ArchiveReviewsBySession(ctx, worker.ID, now))
	if _, created, err := store.CreateReviewerAccountExecution(ctx, old.ID, "refused-pass", target, now); err != nil || created {
		t.Fatalf("active pass was rebuilt: created=%t %v", created, err)
	}
	_, err := s.UpdateReviewRunResult(ctx, run.ID, domain.ReviewRunComplete, domain.VerdictNone, "fake completed review", "", false)
	mustNoError(t, err)
	current, created, err := store.CreateReviewerAccountExecution(ctx, old.ID, "new-execution", target, now.Add(-time.Hour))
	mustNoError(t, err)
	if !created || current.ID == old.ID || current.AgentSessionID != "" || current.ProviderConversationID != "" {
		t.Fatalf("reviewer did not get fresh identity: %+v", current)
	}
	if _, created, err := store.CreateReviewerAccountExecution(ctx, old.ID, "repeat-request", target, now); err != nil || created {
		t.Fatalf("stale rebuild created another owner: created=%t %v", created, err)
	}
	for id, want := range map[string]string{old.ID: "A", current.ID: "B"} {
		binding, found, err := s.GetReviewerAgentAccount(ctx, id, domain.AgentAccountProviderCodex)
		mustNoError(t, err)
		if !found || binding.AccountID != want {
			t.Fatalf("historical identity changed: %s %+v", id, binding)
		}
	}
	stored, found, err := s.GetReviewRun(ctx, run.ID)
	mustNoError(t, err)
	if !found || stored.ReviewID != old.ID {
		t.Fatal("old pass moved to the new account")
	}
	current, found, err = s.GetReviewBySessionAndHarness(ctx, worker.ID, old.Harness)
	mustNoError(t, err)
	if !found || current.ID != "new-execution" {
		t.Fatalf("current execution followed the wall clock: %+v", current)
	}
	rows, err := s.ListReviewsBySession(ctx, worker.ID)
	mustNoError(t, err)
	if len(rows) != 1 || rows[0].ID != current.ID {
		t.Fatalf("current projection includes retired execution: %+v", rows)
	}
}

func TestReviewerRebuildCASAllowsOnlyOneConcurrentReplacement(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	worker := seedUsageSession(t, s, domain.HarnessCodex)
	now := time.Now().UTC()
	old := domain.Review{ID: "archived", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: domain.ReviewerCodex, IsArchived: true, CreatedAt: now, UpdatedAt: now}
	mustNoError(t, s.UpsertReview(ctx, old))
	start := make(chan struct{})
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	for _, id := range []string{"new-A", "new-B"} {
		go func(id string) {
			<-start
			_, created, err := s.CreateReviewerAccountExecution(ctx, old.ID, id, domain.ResolvedAgentAccount{AccountID: id, Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceExplicit}, now)
			results <- created
			errs <- err
		}(id)
	}
	close(start)
	count := 0
	for range 2 {
		if <-results {
			count++
		}
		mustNoError(t, <-errs)
	}
	if count != 1 {
		t.Fatalf("concurrent rebuild admitted %d owners", count)
	}
	rows, err := s.ListReviewsBySession(ctx, worker.ID)
	mustNoError(t, err)
	if len(rows) != 1 || rows[0].ID == old.ID {
		t.Fatalf("current projection ambiguous: %+v", rows)
	}
}
