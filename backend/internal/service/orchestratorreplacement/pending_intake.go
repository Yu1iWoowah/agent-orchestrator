package orchestratorreplacement

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Intake reads current canonical pending rows, not an export or delivery proof.
// No controller, native transcript, report claim or provider action is involved.
func (s *Service) Intake(ctx context.Context, id string) (domain.ReplacementIntake, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return domain.ReplacementIntake{}, err
	}
	return s.deps.Store.ReadReplacementIntake(ctx, id)
}
