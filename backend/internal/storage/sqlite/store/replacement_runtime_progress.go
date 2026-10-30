package store

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type replacementRuntimeProgressKey struct{}

// SaveReplacementRuntimeProgress is the private coordinator publication path
// for retained post-stop reconciliation. It does not authorize retirement,
// owner binding, writer activation or intake release; their producers retain
// separate transaction grants. Generic Save never gets recovery-budget credit
// merely because a caller changes a runtime-looking field.
func (s *Store) SaveReplacementRuntimeProgress(ctx context.Context, op domain.OrchestratorReplacement) (bool, error) {
	if op.SourceStop == nil {
		return false, domain.ErrReplacementStateInvalid
	}
	applied := false
	err := s.replacementTx(ctx, func(ctx context.Context, q *gen.Queries) error {
		var err error
		applied, err = s.SaveOrchestratorReplacement(context.WithValue(ctx, replacementRuntimeProgressKey{}, q), op)
		return err
	})
	return applied && err == nil, err
}
