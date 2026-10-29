package orchestratorreplacement

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func (s *Service) releaseSourceIntake(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	if !op.CanReleaseSourceIntake() || op.SourceIntakeReleasedAt != nil {
		return op, nil
	}
	_, _, actionErr := s.deps.Store.ReleaseReplacementSourceIntake(ctx, op.ID, op.Revision, s.deps.Now().UTC())
	current, err := s.Get(ctx, op.ID)
	if err != nil {
		return op, err
	}
	if current.SourceIntakeReleasedAt != nil {
		return current, nil
	}
	return current, actionErr
}
