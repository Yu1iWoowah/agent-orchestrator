package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func (s *Store) CreateReviewerAccountExecution(ctx context.Context, previousID, newID string, selected domain.ResolvedAgentAccount, at time.Time) (out domain.Review, created bool, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err = s.inTx(ctx, "rebuild reviewer execution", func(q *gen.Queries) error {
		old, e := q.GetReviewByID(ctx, previousID)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		provider, supported := domain.AgentAccountProviderFor(domain.AgentHarness(old.Harness))
		if !supported || provider != selected.Provider {
			return ports.ErrAgentAccountProviderMismatch
		}
		if selected.AccountID == "" {
			return ports.ErrAgentAccountUnknown
		}
		if _, e = q.InsertReviewerAccountExecution(ctx, gen.InsertReviewerAccountExecutionParams{PreviousID: previousID, NewID: newID, CreatedAt: at.UTC()}); errors.Is(e, sql.ErrNoRows) {
			return nil
		} else if e != nil {
			return e
		}
		if e = q.BindReviewerAgentAccount(ctx, gen.BindReviewerAgentAccountParams{ReviewID: newID, Provider: string(selected.Provider), AccountID: selected.AccountID, ResolutionSource: string(selected.Source), BoundAt: at.UTC()}); e != nil {
			return e
		}
		row, e := q.GetReviewByID(ctx, newID)
		if e != nil {
			return e
		}
		out = reviewFromReview(row)
		created = true
		return nil
	})
	return out, created, err
}
