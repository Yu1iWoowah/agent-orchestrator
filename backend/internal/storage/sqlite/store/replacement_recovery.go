package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type replacementRetainedRetryKey struct{}

// RetryRetainedReplacement restores only the persisted reconciliation boundary.
// It cannot accept caller-authored facts, reset a launch intent, or revive source.
func (s *Store) RetryRetainedReplacement(ctx context.Context, id string, revision int64, at, deadline time.Time) (domain.OrchestratorReplacement, bool, error) {
	var result domain.OrchestratorReplacement
	applied := false
	if at.IsZero() || !deadline.After(at) {
		return result, false, domain.ErrReplacementStateInvalid
	}
	err := s.replacementTx(ctx, func(ctx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(ctx, q, row)
		if err != nil {
			return err
		}
		result = op
		if op.Revision != revision || op.Phase != domain.ReplacementRecoveryRequired || op.FrozenAt == nil || op.SourceStopIntentAt == nil || op.Preparation == nil || op.Transcript == nil {
			return domain.ErrReplacementStateInvalid
		}
		phase := op.ResumePhase
		if op.SourceRetiredAt == nil {
			switch phase {
			case domain.ReplacementExportingTranscript, domain.ReplacementTranscriptVerified, domain.ReplacementRetiring:
			default:
				return domain.ErrReplacementStateInvalid
			}
			if op.SuccessorSessionID != "" {
				return domain.ErrReplacementStateInvalid
			}
		} else {
			switch phase {
			case domain.ReplacementStarting, domain.ReplacementControllerReady, domain.ReplacementHandoffSettled, domain.ReplacementVerifyingResume, domain.ReplacementActivatingSharedWriter, domain.ReplacementNotifying:
			default:
				return domain.ErrReplacementStateInvalid
			}
		}
		if phase != domain.ReplacementExportingTranscript && (op.SourceStop == nil || op.Transcript.Receipt == nil) {
			return domain.ErrReplacementStateInvalid
		}
		if matches, err := replacementProjectMatches(ctx, q, op); err != nil {
			return err
		} else if !matches {
			return domain.ErrReplacementConfigChanged
		}
		source, err := q.GetSession(ctx, op.SourceSessionID)
		if err != nil {
			return err
		}
		rec := rowToRecord(source)
		owner := op.SourceOwner
		owner.IsTerminated = op.SourceRetiredAt != nil
		if rec.ProjectID != op.ProjectID || rec.ControllerOwner() != owner || rec.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
			return domain.ErrReplacementSourceChanged
		}
		if op.SourceRetiredAt == nil && op.SourceStop != nil && !op.SourceStop.MatchesSource(rec) {
			return domain.ErrReplacementSourceChanged
		}
		candidate := op
		candidate.Phase = phase
		if op.SuccessorSessionID != "" {
			target, err := q.GetSession(ctx, op.SuccessorSessionID)
			if err != nil {
				return err
			}
			t := rowToRecord(target)
			if t.IsTerminated || t.Kind != domain.KindOrchestrator || t.ProjectID != op.ProjectID || t.Harness != domain.HarnessClaudeCode || t.Mode != domain.SessionModeChat || t.ClientRequestID != op.SpawnRequestID || t.ClientRequestHash != op.SpawnRequestHash || phase != domain.ReplacementNotifying && (t.Metadata.Model != op.TargetAgentConfig.Model || t.Metadata.Effort != op.TargetAgentConfig.Effort || t.Metadata.Permissions != op.TargetAgentConfig.Permissions) {
				return domain.ErrReplacementSourceChanged
			}
			proof, verified := candidate.ExpectedSuccessorController()
			if phase == domain.ReplacementNotifying {
				matches, err := replacementActivatedOwnerMatches(ctx, q, candidate, t)
				if err != nil {
					return err
				}
				if !matches {
					return domain.ErrReplacementSourceChanged
				}
				proof.Owner, proof.CapabilityVerifier, verified = t.ControllerOwner(), t.Metadata.BrowserCapabilityVerifier, true
			}
			if verified {
				if t.ControllerOwner() != proof.Owner || t.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
					return domain.ErrReplacementSourceChanged
				}
			} else if phase != domain.ReplacementStarting || candidate.SuccessorController != nil || t.Metadata.ControllerGeneration != "" || t.Metadata.ProviderConversationID != "" {
				return domain.ErrReplacementSourceChanged
			}
		} else if op.SourceRetiredAt != nil && phase != domain.ReplacementStarting {
			return domain.ErrReplacementStateInvalid
		}
		controlRetry, err := retryReplacementControl(ctx, q, op, &candidate, at)
		if err != nil {
			return err
		}
		if controlRetry {
			ctx = context.WithValue(ctx, replacementControlRetryKey{}, q)
		}
		candidate.ResumePhase, candidate.Error = "", nil
		candidate.UpdatedAt, candidate.DeadlineAt = at.UTC(), deadline.UTC()
		if op.Error != nil && !controlRetry {
			fingerprint, err := domain.ReplacementReconciliationHash(candidate)
			if err != nil {
				return err
			}
			candidate.RetainedRetry = &domain.ReplacementRetainedRetry{Phase: phase, FactsSHA256: fingerprint, Failure: *op.Error, RequestedAt: at.UTC()}
		}
		if attempt, ok := candidate.LatestSpawnAttempt(); phase == domain.ReplacementStarting && ok && attempt.LaunchIntentAt == nil {
			candidate.SpawnRetryGeneration++
		}
		saved, err := s.SaveOrchestratorReplacement(context.WithValue(ctx, replacementRetainedRetryKey{}, q), candidate)
		if err != nil {
			return err
		}
		if !saved {
			return domain.ErrReplacementSourceChanged
		}
		candidate.Revision++
		result, applied = candidate, true
		return nil
	})
	return result, applied && err == nil, err
}
