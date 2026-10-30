package store

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// BeginReplacementSpawnAttempt reserves preparation of the one bound successor.
// A new attempt after failure requires explicit Retry authorization and no
// durable launch intent. An obsolete background task cannot claim that intent.
func (s *Store) BeginReplacementSpawnAttempt(ctx context.Context, operation string, session domain.SessionID, attemptID string, at time.Time) (domain.OrchestratorReplacement, domain.SessionRecord, error) {
	var result domain.OrchestratorReplacement
	var target domain.SessionRecord
	err := s.replacementTx(ctx, func(ctx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(ctx, operation)
		if err != nil {
			return err
		}
		op, err := decodeReplacement(row)
		if err != nil {
			return err
		}
		target, err = replacementSpawnSeed(ctx, q, op, session, true)
		if err != nil {
			return err
		}
		if at.IsZero() || attemptID == "" || target.Metadata.ControllerGeneration != "" || target.Metadata.ProviderConversationID != "" || target.Metadata.RuntimeHandleID != "" || target.Metadata.RuntimeLaunchID != "" {
			return domain.ErrReplacementStateInvalid
		}
		for _, attempt := range op.SpawnAttempts {
			if attempt.ID == attemptID {
				return domain.ErrReplacementStateInvalid
			}
		}
		now := at.UTC()
		if prior, exists := op.LatestSpawnAttempt(); exists {
			if prior.LaunchIntentAt != nil || prior.RetryGeneration >= op.SpawnRetryGeneration {
				return domain.ErrReplacementStateInvalid
			}
			if prior.FailedAt == nil {
				op.SpawnAttempts[len(op.SpawnAttempts)-1].FailedAt = &now
			}
		} else if target.ProvisionState == domain.SessionProvisionFailed {
			return domain.ErrReplacementStateInvalid
		}
		op.SpawnAttempts = append(op.SpawnAttempts, domain.ReplacementSpawnAttempt{ID: attemptID, RetryGeneration: op.SpawnRetryGeneration, StartedAt: now})
		changed, err := q.SetSessionProvisionState(ctx, gen.SetSessionProvisionStateParams{ID: session, ProvisionState: domain.SessionProvisionProvisioning, UpdatedAt: now})
		if err != nil {
			return err
		}
		if changed != 1 {
			return domain.ErrReplacementStateInvalid
		}
		if err := s.saveReplacementSpawnAttempt(ctx, q, &op, now); err != nil {
			return err
		}
		targetRow, err := q.GetSession(ctx, session)
		if err != nil {
			return err
		}
		result, target = op, rowToRecord(targetRow)
		return nil
	})
	return result, target, err
}

// MarkReplacementSpawnLaunch consumes the attempt's single launch permission.
// A duplicate/lost receipt never authorizes calling the provider a second time.
func (s *Store) MarkReplacementSpawnLaunch(ctx context.Context, operation string, session domain.SessionID, attemptID string, at time.Time) (bool, error) {
	return s.changeReplacementSpawnAttempt(ctx, operation, session, attemptID, at, nil)
}

// FailReplacementSpawnAttempt records failure without stopping any controller.
// Attempt identity prevents a late error from overwriting newer preparation.
func (s *Store) FailReplacementSpawnAttempt(ctx context.Context, operation string, session domain.SessionID, attemptID, message string, at time.Time) (bool, error) {
	return s.changeReplacementSpawnAttempt(ctx, operation, session, attemptID, at, &message)
}

func (s *Store) changeReplacementSpawnAttempt(ctx context.Context, operation string, session domain.SessionID, attemptID string, at time.Time, failure *string) (bool, error) {
	applied := false
	err := s.replacementTx(ctx, func(ctx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(ctx, operation)
		if err != nil {
			return err
		}
		op, err := decodeReplacement(row)
		if err != nil {
			return err
		}
		target, err := replacementSpawnSeed(ctx, q, op, session, failure == nil)
		if err != nil {
			return err
		}
		attempt, exists := op.LatestSpawnAttempt()
		if !exists || at.IsZero() || attempt.ID != attemptID || attempt.RetryGeneration != op.SpawnRetryGeneration {
			return domain.ErrReplacementStateInvalid
		}
		if proof := op.SuccessorController; proof != nil {
			if target.ControllerOwner() != proof.Owner || target.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
				return domain.ErrReplacementSourceChanged
			}
		} else if target.Metadata.ControllerGeneration != "" || target.Metadata.ProviderConversationID != "" {
			return domain.ErrReplacementSourceChanged
		}
		now := at.UTC()
		last := &op.SpawnAttempts[len(op.SpawnAttempts)-1]
		if failure == nil {
			if attempt.LaunchIntentAt != nil || attempt.FailedAt != nil || op.SuccessorController != nil || target.Metadata.RuntimeHandleID != "" || target.Metadata.RuntimeLaunchID != "" {
				return nil
			}
			last.LaunchIntentAt = &now
		} else {
			if attempt.FailedAt != nil {
				applied = true
				return nil
			}
			last.FailedAt = &now
			changed, err := q.SetSessionProvisionState(ctx, gen.SetSessionProvisionStateParams{ID: session, ProvisionState: domain.SessionProvisionFailed, ProvisionError: *failure, UpdatedAt: now})
			if err != nil {
				return err
			}
			if changed != 1 {
				return domain.ErrReplacementStateInvalid
			}
		}
		if err := s.saveReplacementSpawnAttempt(ctx, q, &op, now); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied && err == nil, err
}

func (s *Store) saveReplacementSpawnAttempt(ctx context.Context, q *gen.Queries, op *domain.OrchestratorReplacement, at time.Time) error {
	op.UpdatedAt = at
	saved, err := s.SaveOrchestratorReplacement(context.WithValue(ctx, replacementSpawnAttemptKey{}, q), *op)
	if err != nil {
		return err
	}
	if !saved {
		return domain.ErrReplacementStateInvalid
	}
	op.Revision++
	return nil
}

func replacementSpawnSeed(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement, session domain.SessionID, requireConfig bool) (domain.SessionRecord, error) {
	var zero domain.SessionRecord
	if op.Phase != domain.ReplacementStarting || op.SourceRetiredAt == nil || op.SuccessorSessionID != session || op.SpawnRequestID == "" || op.SpawnRequestHash == "" {
		return zero, domain.ErrReplacementStateInvalid
	}
	sourceRow, err := q.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		return zero, err
	}
	source := rowToRecord(sourceRow)
	owner := op.SourceOwner
	owner.IsTerminated = true
	if source.ProjectID != op.ProjectID || source.ControllerOwner() != owner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return zero, domain.ErrReplacementSourceChanged
	}
	if requireConfig {
		if matches, err := replacementProjectMatches(ctx, q, op); err != nil {
			return zero, err
		} else if !matches {
			return zero, domain.ErrReplacementConfigChanged
		}
	}

	row, err := q.GetSession(ctx, session)
	if err != nil {
		return zero, err
	}
	target := rowToRecord(row)
	if target.IsTerminated || target.ProjectID != op.ProjectID || target.Kind != domain.KindOrchestrator || target.Harness != domain.HarnessClaudeCode || target.Mode != domain.SessionModeChat || target.ClientRequestID != op.SpawnRequestID || target.ClientRequestHash != op.SpawnRequestHash || target.Metadata.Model != op.TargetAgentConfig.Model || target.Metadata.Effort != op.TargetAgentConfig.Effort || target.Metadata.Permissions != op.TargetAgentConfig.Permissions {
		return zero, domain.ErrReplacementSourceChanged
	}
	return target, nil
}
