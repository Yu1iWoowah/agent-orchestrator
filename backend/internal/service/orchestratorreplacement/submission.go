package orchestratorreplacement

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

// Prepared accepts an authenticated author receipt. Only a later independently
// observed successful control turn may mark the preparation verified.
func (s *Service) Prepared(ctx context.Context, id string, in projectdocuments.ReplacementPreparationInput) (domain.OrchestratorReplacement, error) {
	op, err := s.Get(ctx, id)
	if err != nil {
		return op, err
	}
	if op.Preparation == nil || s.deps.Documents == nil {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_NOT_PREPARING", "No active preparation exists", nil)
	}
	p := op.Preparation
	expected := PreparationMetadata{Version: 1, OperationID: op.ID, AttemptID: p.AttemptID, Nonce: p.Nonce, SourceSessionID: op.SourceSessionID, LeaseEpoch: p.LeaseEpoch}
	return s.deps.Documents.RecordReplacementPreparation(ctx, id, op.Revision, in, func(file projectdocuments.File) error {
		if err := ValidatePreparation([]byte(file.Content), expected, p.BaselineHash, p.ExpectedSessions); err != nil {
			return apierr.Conflict("ORCHESTRATOR_HANDOFF_CONTENT_INVALID", err.Error(), nil)
		}
		return nil
	})
}
