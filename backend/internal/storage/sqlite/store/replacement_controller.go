package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// Called only in the same transaction as the native owner/branch publication.
func (s *Store) bindReplacementSuccessorController(ctx context.Context, q *gen.Queries, before, ready domain.SessionRecord) error {
	row, err := q.GetActiveOrchestratorReplacement(ctx, string(ready.ProjectID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if ready.Kind != domain.KindOrchestrator || ready.ID == domain.SessionID(row.SourceSessionID) {
		return nil
	}
	// Route unrelated controllers using sealed identity, never an unverified
	// successorSessionId from the mutable row. Corruption must remain fenced
	// without blocking ordinary Chat commits elsewhere in the project.
	checkpoint, err := q.GetLatestReplacementRepairSnapshot(ctx, row.ID)
	if err == nil {
		sealed, err := checkpointRow(checkpoint)
		if err != nil {
			return err
		}
		trusted, err := decodeReplacement(sealed)
		if err != nil {
			return err
		}
		if trusted.SuccessorSessionID != ready.ID {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	op, err := s.decodeCheckpointedReplacement(ctx, q, row)
	if err != nil {
		return err
	}
	if op.SuccessorSessionID != ready.ID {
		return nil
	}
	if op.SuccessorController != nil {
		return nil
	} // A later epoch cannot rewrite the first receipt.
	if op.Phase != domain.ReplacementStarting || op.SourceRetiredAt == nil || before.ClientRequestID == "" || before.ClientRequestID != op.SpawnRequestID || before.ClientRequestHash != op.SpawnRequestHash || before.Metadata.ProviderConversationID != "" || ready.Metadata.BrowserCapabilityVerifier == "" || ready.Metadata.BrowserCapabilityVerifier != before.Metadata.BrowserCapabilityVerifier || ready.Metadata.Model != op.TargetAgentConfig.Model || ready.Metadata.Effort != op.TargetAgentConfig.Effort || ready.Metadata.Permissions != op.TargetAgentConfig.Permissions {
		return domain.ErrReplacementSourceChanged
	}
	if matches, err := replacementProjectMatches(ctx, q, op); err != nil {
		return err
	} else if !matches {
		return domain.ErrReplacementConfigChanged
	}
	op.SuccessorController = &domain.ReplacementSuccessorController{Owner: ready.ControllerOwner(), CapabilityVerifier: ready.Metadata.BrowserCapabilityVerifier, CommittedAt: ready.UpdatedAt}
	if err := validateReplacementSuccessorController(op); err != nil {
		return err
	}
	op.UpdatedAt = ready.UpdatedAt
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	count, err := q.UpdateOrchestratorReplacement(ctx, gen.UpdateOrchestratorReplacementParams{ID: op.ID, Revision: op.Revision, Phase: string(op.Phase), IsActive: true, StateJson: string(data), UpdatedAt: op.UpdatedAt.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	if count != 1 {
		return domain.ErrReplacementSourceChanged
	}
	return s.saveReplacementCheckpoint(replacementRecoveryCheckpointContext(ctx, q), q, op.ID)
}
