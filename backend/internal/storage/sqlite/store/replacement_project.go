package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func replacementProjectMatches(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement) (bool, error) {
	project, err := q.GetProject(ctx, op.ProjectID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if project.ArchivedAt.Valid {
		return false, nil
	}
	if op.ProjectFingerprint == "" {
		return true, nil
	}
	fingerprint, err := domain.ReplacementProjectFingerprint(projectRowFromGen(project))
	return fingerprint == op.ProjectFingerprint, err
}
