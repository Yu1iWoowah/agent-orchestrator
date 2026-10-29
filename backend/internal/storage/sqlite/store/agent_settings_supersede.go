package store

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// SupersedeQueuedAgentSettingsOperation atomically ends an untouched local
// intention and accepts its merged replacement, including both audit records.
func (s *Store) SupersedeQueuedAgentSettingsOperation(ctx context.Context, oldID string, version int64, next domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	if next.ApplyMode == "" {
		next.ApplyMode = domain.AgentSettingsAfterTurn
	}
	if next.Checkpoint == "" {
		next.Checkpoint = domain.AgentSettingsQueued
	}
	if err := validateSettingsOperation(next); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	if next.Actor.Kind != domain.AgentSettingsActorLocal || next.Authority != (domain.AgentSettingsAuthority{}) {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsConflict
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.AgentSettingsOperation
	err := s.inTx(ctx, "supersede local agent settings", func(q *gen.Queries) error {
		row, err := q.GetAgentSettingsOperation(ctx, gen.GetAgentSettingsOperationParams{SessionID: string(next.SessionID), ID: oldID})
		if err != nil {
			return settingsStoreError(err)
		}
		old, err := decodeSettingsOperation(row)
		if err != nil {
			return err
		}
		if !old.CanSupersedeLocally() || old.Version != version || old.ID == next.ID || old.IdempotencyKey == next.IdempotencyKey ||
			old.Source != next.Source || old.BaseRevision != next.BaseRevision || old.AccountID != next.AccountID ||
			old.ControllerGeneration != next.ControllerGeneration || old.ProviderConversationID != next.ProviderConversationID || old.BranchID != next.BranchID {
			return ports.ErrAgentSettingsConflict
		}
		old.ErrorCode = "AGENT_SETTINGS_SUPERSEDED"
		if _, err := setSettingsOutcome(ctx, q, old, domain.AgentSettingsCancelled, domain.AgentSettingsErrorCancelled, next.CreatedAt); err != nil {
			return err
		}
		result, err = beginAgentSettingsOperationTx(ctx, q, next)
		return err
	})
	return result, err
}
