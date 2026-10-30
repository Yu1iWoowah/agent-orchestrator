package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type replacementResumeUnlaunchedKey struct{}

// RecordReplacementResumeUnlaunched accepts the runtime's positive refusal
// before launcher entry. It cannot clear an uncertain launch or a new owner.
func (s *Store) RecordReplacementResumeUnlaunched(ctx context.Context, id string, revision int64, generation, reason string, at time.Time) (domain.OrchestratorReplacement, bool, error) {
	var result domain.OrchestratorReplacement
	applied := false
	if generation == "" || at.IsZero() {
		return result, false, domain.ErrReplacementStateInvalid
	}
	if len(reason) > 4096 {
		reason = reason[:4096]
	}
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(tx, q, row)
		if err != nil {
			return err
		}
		result = op
		p := op.ResumeEvidence
		if p != nil && op.Phase == domain.ReplacementRecoveryRequired && op.ResumePhase == domain.ReplacementVerifyingResume && p.ResumeIntentAt == nil && p.ResumeGeneration == "" && p.ResumedAt == nil && len(p.UnlaunchedAttempts) > 0 && p.UnlaunchedAttempts[len(p.UnlaunchedAttempts)-1].Generation == generation {
			applied = true // The previous transaction committed but lost its reply.
			return nil
		}
		if op.Revision != revision || op.Phase != domain.ReplacementVerifyingResume || op.SourceRetiredAt == nil || p == nil || !p.StopConfirmed || p.StopReceipt == nil || p.ResumeIntentAt == nil || p.ResumeGeneration != generation || p.ResumedAt != nil || p.Probe != nil || at.Before(*p.ResumeIntentAt) {
			return nil
		}
		targetRow, err := q.GetSession(tx, op.SuccessorSessionID)
		if err != nil {
			return err
		}
		target := rowToRecord(targetRow)
		if target.ProjectID != op.ProjectID || target.Kind != domain.KindOrchestrator || target.ControllerOwner() != p.BeforeOwner || target.Metadata.BrowserCapabilityVerifier != p.BeforeCapabilityVerifier || !p.StopReceipt.MatchesSource(target) {
			return nil
		}
		next := *p
		next.UnlaunchedAttempts = append(slices.Clone(p.UnlaunchedAttempts), domain.ReplacementResumeUnlaunched{Generation: generation, IntentAt: *p.ResumeIntentAt, ConfirmedAt: at.UTC()})
		next.ResumeIntentAt, next.ResumeGeneration = nil, ""
		op.ResumeEvidence = &next
		op.Phase, op.ResumePhase = domain.ReplacementRecoveryRequired, domain.ReplacementVerifyingResume
		op.Error = &domain.ReplacementFailure{Code: "ORCHESTRATOR_HANDOFF_RESUME_NOT_LAUNCHED", Message: reason, Retryable: true}
		op.UpdatedAt = at.UTC()
		saved, err := s.SaveOrchestratorReplacement(context.WithValue(tx, replacementResumeUnlaunchedKey{}, q), op)
		if err != nil || !saved {
			return err
		}
		op.Revision++
		result, applied = op, true
		return nil
	})
	return result, applied && err == nil, err
}
