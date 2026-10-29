package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func replacementQueueError(err error) error {
	if err != nil && strings.Contains(err.Error(), "ORCHESTRATOR_REPLACEMENT_INTAKE_FENCED") {
		return domain.ErrReplacementIntakeFenced
	}
	return err
}

func (s *Store) validateUndispatchedConversation(ctx context.Context, q *gen.Queries, id domain.SessionID, conversationID string) error {
	row, err := q.GetSession(ctx, id)
	if err != nil {
		return fmt.Errorf("check queued session %s: %w", id, err)
	}
	if row.IsTerminated {
		return domain.ErrSessionNotProvisioning
	}
	if !row.ProvisionState.IsProvisioning() && !row.HibernatedAt.Valid {
		rec := rowToRecord(row)
		if rec.Kind != domain.KindOrchestrator {
			return domain.ErrSessionNotProvisioning
		}
		operation, err := q.GetActiveOrchestratorReplacement(ctx, string(rec.ProjectID))
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrSessionNotProvisioning
		}
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(ctx, q, operation)
		if err != nil {
			return err
		}
		source := op.SourceRetiredAt == nil && rec.ID == op.SourceSessionID && rec.ControllerOwner() == op.SourceOwner && op.SourceCapabilityVerifier != "" && rec.Metadata.BrowserCapabilityVerifier == op.SourceCapabilityVerifier
		successor := op.SourceRetiredAt != nil && rec.Mode == domain.SessionModeChat && (rec.ID == op.SuccessorSessionID || op.SpawnRequestID != "" && op.SpawnRequestHash != "" && rec.ClientRequestID == op.SpawnRequestID && rec.ClientRequestHash == op.SpawnRequestHash)
		if rec.Harness != domain.HarnessClaudeCode || (!source && !successor) {
			return domain.ErrReplacementSourceChanged
		}
	}
	conversation, err := q.SelectConversationByID(ctx, conversationID)
	if err != nil {
		return fmt.Errorf("check queued conversation %s: %w", conversationID, err)
	}
	if conversation.CurrentSessionID == nil || *conversation.CurrentSessionID != id {
		return domain.ErrConversationRebound
	}
	return nil
}
