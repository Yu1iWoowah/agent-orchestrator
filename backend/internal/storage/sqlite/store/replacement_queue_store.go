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

// starting accepts a session whose controller the caller is starting: that
// start delivers the input, so it need not be provisioning or hibernated.
func (s *Store) validateUndispatchedConversation(ctx context.Context, q *gen.Queries, id domain.SessionID, conversationID string, starting bool) error {
	row, err := q.GetSession(ctx, id)
	if err != nil {
		return fmt.Errorf("check queued session %s: %w", id, err)
	}
	if row.IsTerminated {
		return domain.ErrSessionNotProvisioning
	}
	if !row.ProvisionState.IsProvisioning() && !row.HibernatedAt.Valid && !starting {
		move, moveErr := q.GetActiveSessionInterfaceTransition(ctx, id)
		if moveErr != nil && !errors.Is(moveErr, sql.ErrNoRows) {
			return fmt.Errorf("check account move of %s: %w", id, moveErr)
		}
		awaitingTarget := moveErr == nil && interfaceTransitionQueryToDomain(gen.GetSessionInterfaceTransitionRow(move)).AccountMoveAwaitsTarget()
		rec := rowToRecord(row)
		if rec.Kind != domain.KindOrchestrator {
			if !awaitingTarget {
				return domain.ErrSessionNotProvisioning
			}
		} else {
			operation, err := q.GetActiveOrchestratorReplacement(ctx, string(rec.ProjectID))
			if errors.Is(err, sql.ErrNoRows) {
				if !awaitingTarget {
					return domain.ErrSessionNotProvisioning
				}
			} else if err != nil {
				return err
			} else {
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
