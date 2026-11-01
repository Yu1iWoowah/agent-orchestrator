package store

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// PageGuardianFacts reads a fixed-size page without collection, file access or
// writes. The service selects the explicit public whitelist from these rows.
func (s *Store) PageGuardianFacts(ctx context.Context, id domain.SessionID, since, until, floor time.Time, kind, after string) ([]domain.GuardianFact, error) {
	facts := make([]domain.GuardianFact, 0)
	if kind == "request" {
		rows, err := s.qr.PageGuardianRequests(ctx, gen.PageGuardianRequestsParams{SessionID: string(id), SinceAt: since.UTC(), UntilAt: until.UTC(), RetentionFloor: floor.UTC(), AfterSequence: after})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			request := domain.GuardianRequest{CLIVersion: r.CliVersion, EpochID: r.EpochID, ThreadID: r.ThreadID, TurnID: r.TurnID, ResponseID: r.ResponseID, RootTurnID: r.RootTurnID.String, ModelID: r.ModelID.String, Tokens: domain.GuardianTokens{Input: guardianPtr(r.InputTokens), Cached: guardianPtr(r.CachedTokens), CacheWrite: guardianPtr(r.CacheWriteTokens), Output: guardianPtr(r.OutputTokens), Reasoning: guardianPtr(r.ReasoningTokens), Total: guardianPtr(r.TotalTokens)}, ProviderAt: r.ProviderAt, FirstSeenAt: r.FirstSeenAt, Conflict: r.Conflict != 0}
			facts = append(facts, domain.GuardianFact{Kind: kind, Sequence: r.Sequence, Request: &request})
		}
	} else {
		rows, err := s.qr.PageGuardianReviews(ctx, gen.PageGuardianReviewsParams{SessionID: string(id), SinceAt: since.UTC(), UntilAt: until.UTC(), RetentionFloor: floor.UTC(), AfterSequence: after})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			review := guardianReview(gen.GuardianReview{EpochID: r.EpochID, ParentThreadID: r.ParentThreadID, ReviewID: r.ReviewID, ParentTurnID: r.ParentTurnID, TargetItemID: r.TargetItemID, Action: r.Action, Status: r.Status, DecisionSource: r.DecisionSource, StartedAt: r.StartedAt, CompletedAt: r.CompletedAt, FirstSeenAt: r.FirstSeenAt, RetentionAt: r.RetentionAt, SawStarted: r.SawStarted, SawCompleted: r.SawCompleted, Conflict: r.Conflict})
			facts = append(facts, domain.GuardianFact{Kind: kind, Sequence: r.Sequence, Review: &review})
		}
	}
	return facts, nil
}
