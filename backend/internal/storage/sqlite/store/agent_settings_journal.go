package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// PendingAgentSettingsOperation returns the unfinished intention, including a
// quarantined recovery. A missing intention is a normal read, not a mutation.
func (s *Store) PendingAgentSettingsOperation(ctx context.Context, id domain.SessionID) (domain.AgentSettingsOperation, bool, error) {
	row, err := s.qr.GetPendingAgentSettingsOperation(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AgentSettingsOperation{}, false, nil
	}
	if err != nil {
		return domain.AgentSettingsOperation{}, false, err
	}
	op, err := decodeSettingsOperation(row)
	return op, err == nil, err
}

// setSettingsOutcome is called only inside the caller's write transaction.
func setSettingsOutcome(ctx context.Context, q *gen.Queries, op domain.AgentSettingsOperation, phase domain.AgentSettingsPhase, category domain.AgentSettingsErrorCategory, at time.Time) (domain.AgentSettingsOperation, error) {
	changed, err := q.SetAgentSettingsOperationPhase(ctx, gen.SetAgentSettingsOperationPhaseParams{SessionID: string(op.SessionID), ID: op.ID, ExpectedVersion: op.Version, Phase: string(phase), ErrorCategory: string(category), At: at, FinishedAt: sql.NullTime{Time: at, Valid: phase != domain.AgentSettingsRecoveryRequired}})
	if err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	if changed != 1 {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsConflict
	}
	if err := q.InsertAgentSettingsAudit(ctx, gen.InsertAgentSettingsAuditParams{OperationID: op.ID, Phase: string(phase), RecordedAt: at, ErrorCategory: string(category), RequestID: op.RequestID}); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	op.Phase, op.ErrorCategory, op.UpdatedAt = phase, category, at
	op.Version++
	if phase != domain.AgentSettingsRecoveryRequired {
		op.FinishedAt = &at
	}
	return op, nil
}

// FinishAgentSettingsOperation never calls a provider. Cancellation is limited
// to an unstarted intention; uncertain provider effects retain the active fence.
func (s *Store) FinishAgentSettingsOperation(ctx context.Context, id domain.SessionID, operationID string, expectedVersion int64, phase domain.AgentSettingsPhase, category domain.AgentSettingsErrorCategory, at time.Time) (domain.AgentSettingsOperation, error) {
	if !category.Valid() || at.IsZero() || expectedVersion < 0 {
		return domain.AgentSettingsOperation{}, fmt.Errorf("invalid settings terminal outcome")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.AgentSettingsOperation
	err := s.inTx(ctx, "finish agent settings", func(q *gen.Queries) error {
		row, err := q.GetAgentSettingsOperation(ctx, gen.GetAgentSettingsOperationParams{SessionID: string(id), ID: operationID})
		if err != nil {
			return settingsStoreError(err)
		}
		op, err := decodeSettingsOperation(row)
		if err != nil {
			return err
		}
		if op.Phase == phase && op.ErrorCategory == category && op.Version == expectedVersion+1 {
			result = op
			return nil
		}
		if op.Version != expectedVersion || (op.Phase != domain.AgentSettingsPending && op.Phase != domain.AgentSettingsRecoveryRequired) {
			return ports.ErrAgentSettingsConflict
		}
		safe := op.Checkpoint == domain.AgentSettingsQueued && op.StartedAt == nil || op.Checkpoint == domain.AgentSettingsRestored
		switch phase {
		case domain.AgentSettingsCancelled:
			if op.Checkpoint != domain.AgentSettingsQueued || op.StartedAt != nil || category != domain.AgentSettingsErrorCancelled {
				return ports.ErrAgentSettingsConflict
			}
		case domain.AgentSettingsFailed:
			if !safe || category == domain.AgentSettingsErrorCancelled {
				return ports.ErrAgentSettingsConflict
			}
			if op.Phase == domain.AgentSettingsRecoveryRequired {
				current, err := q.GetSessionAgentSettings(ctx, id)
				if err != nil {
					return settingsStoreError(err)
				}
				if op.Recovery == nil || !settingsRecoveryMatches(current, *op.Recovery) {
					return ports.ErrAgentSettingsConflict
				}
			}
		case domain.AgentSettingsRecoveryRequired:
			if safe || op.StartedAt == nil || category == domain.AgentSettingsErrorCancelled || op.Phase == phase {
				return ports.ErrAgentSettingsConflict
			}
			current, err := q.GetSessionAgentSettings(ctx, id)
			if err != nil {
				return settingsStoreError(err)
			}
			if !settingsOwnerCanRecover(current) {
				return errSettingsOwnerUnrecoverable
			}
		default:
			return fmt.Errorf("invalid settings terminal phase")
		}
		result, err = setSettingsOutcome(ctx, q, op, phase, category, at)
		return err
	})
	return result, err
}

// AdvanceAgentSettingsOperation durably fences an action before any provider
// call. A stale CAS must inspect the saved checkpoint instead of authorizing
// the provider action a second time.
func (s *Store) AdvanceAgentSettingsOperation(ctx context.Context, id domain.SessionID, operationID string, expectedVersion int64, checkpoint domain.AgentSettingsCheckpoint, at time.Time) (domain.AgentSettingsOperation, error) {
	if at.IsZero() || expectedVersion < 0 {
		return domain.AgentSettingsOperation{}, fmt.Errorf("invalid settings checkpoint")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.AgentSettingsOperation
	err := s.inTx(ctx, "advance agent settings", func(q *gen.Queries) error {
		row, err := q.GetAgentSettingsOperation(ctx, gen.GetAgentSettingsOperationParams{SessionID: string(id), ID: operationID})
		if err != nil {
			return settingsStoreError(err)
		}
		op, err := decodeSettingsOperation(row)
		if err != nil {
			return err
		}
		if op.Phase != domain.AgentSettingsPending {
			return ports.ErrAgentSettingsConflict
		}
		if op.Version != expectedVersion || !op.Checkpoint.CanAdvanceTo(checkpoint) {
			return ports.ErrAgentSettingsConflict
		}
		current, err := q.GetSessionAgentSettings(ctx, id)
		if err != nil {
			return settingsStoreError(err)
		}
		if !settingsOwnerMatches(current, op) {
			return ports.ErrAgentSettingsConflict
		}
		if op.StartedAt == nil && !settingsOwnerCanRecover(current) {
			return errSettingsOwnerUnrecoverable
		}
		changed, err := q.AdvanceAgentSettingsCheckpoint(ctx, gen.AdvanceAgentSettingsCheckpointParams{SessionID: string(id), ID: operationID, ExpectedVersion: expectedVersion, Checkpoint: string(checkpoint), At: at})
		if err != nil {
			return err
		}
		if changed != 1 {
			return ports.ErrAgentSettingsConflict
		}
		op.Checkpoint, op.UpdatedAt = checkpoint, at
		op.Version++
		if op.StartedAt == nil {
			op.StartedAt = &at
		}
		result = op
		return nil
	})
	return result, err
}
