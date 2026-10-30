package store

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// WithReplacementSpawnPublication fences only short DB publications. Worktree
// creation, setup hooks and provider I/O must stay outside this reservation.
func (s *Store) WithReplacementSpawnPublication(ctx context.Context, operation string, id domain.SessionID, attemptID string, publish func(context.Context) error) error {
	if publish == nil || attemptID == "" {
		return domain.ErrReplacementStateInvalid
	}
	return s.replacementTx(ctx, func(ctx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(ctx, operation)
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(ctx, q, row)
		if err != nil {
			return err
		}
		switch op.Phase {
		case domain.ReplacementStarting, domain.ReplacementControllerReady, domain.ReplacementHandoffSettled:
		default:
			return domain.ErrReplacementStateInvalid
		}
		attempt, ok := op.LatestSpawnAttempt()
		if !ok || attempt.ID != attemptID || attempt.RetryGeneration != op.SpawnRetryGeneration || attempt.FailedAt != nil || op.SourceRetiredAt == nil || op.SuccessorSessionID != id {
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
		owner := op.SourceOwner
		owner.IsTerminated = true
		if sourceRec := rowToRecord(source); sourceRec.ControllerOwner() != owner || sourceRec.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
			return domain.ErrReplacementSourceChanged
		}
		target, err := q.GetSession(ctx, id)
		if err != nil {
			return err
		}
		rec := rowToRecord(target)
		if rec.IsTerminated || rec.HibernatedAt != nil || rec.ProjectID != op.ProjectID || rec.Kind != domain.KindOrchestrator || rec.Harness != domain.HarnessClaudeCode || rec.Mode != domain.SessionModeChat || rec.ClientRequestID != op.SpawnRequestID || rec.ClientRequestHash != op.SpawnRequestHash {
			return domain.ErrReplacementSourceChanged
		}
		if proof := op.SuccessorController; proof != nil {
			if rec.ControllerOwner() != proof.Owner || rec.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
				return domain.ErrReplacementSourceChanged
			}
		} else if rec.Metadata.ControllerGeneration != "" || rec.Metadata.ProviderConversationID != "" {
			return domain.ErrReplacementSourceChanged
		}
		return publish(ctx)
	})
}

func (s *Store) replacementMutationWriter(ctx context.Context) (*gen.Queries, func()) {
	if q := s.replacementReader(ctx); q != s.qr {
		return q, func() {}
	}
	s.writeMu.Lock()
	return s.qw, s.writeMu.Unlock
}
