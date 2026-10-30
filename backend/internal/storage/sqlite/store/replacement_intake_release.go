package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type replacementIntakeReleaseKey struct{}
type replacementIntakeDrainKey struct{}

// ReleaseReplacementIntake publishes the drain boundary only for the resumed
// writer. The canonical queue and pending notification receipts remain intact.
func (s *Store) ReleaseReplacementIntake(ctx context.Context, id string, revision int64, at time.Time) (domain.OrchestratorReplacement, bool, error) {
	var result domain.OrchestratorReplacement
	applied := false
	if at.IsZero() {
		return result, false, domain.ErrReplacementStateInvalid
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
		if op.IntakeReleasedAt != nil {
			applied = true
			return nil
		}
		if op.Revision != revision || op.Phase != domain.ReplacementNotifying || op.WriterActivation == nil {
			return nil
		}
		target, conversation, err := verifiedReplacementIntakeWriter(tx, q, op)
		if errors.Is(err, domain.ErrReplacementIntakeFenced) {
			return nil
		}
		if err != nil {
			return err
		}

		now := at.UTC()
		op.IntakeTransfer, err = transferReplacementIntake(tx, q, op, target, conversation, now)
		if err != nil {
			return err
		}
		op.IntakeReleasedAt = &now
		op.UpdatedAt = now
		saved, err := s.SaveOrchestratorReplacement(context.WithValue(tx, replacementIntakeReleaseKey{}, q), op)
		if err != nil {
			return err
		}
		if !saved {
			return domain.ErrReplacementStateInvalid
		}
		if err := restoreReplacementHumanProjection(tx, q, target.ID); err != nil {
			return err
		}
		op.Revision++
		result = op
		applied = true
		return nil
	})
	return result, applied && err == nil, err
}

func preserveReplacementIntakeRelease(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	if before.IntakeReleasedAt != nil {
		if after.IntakeReleasedAt == nil || !before.IntakeReleasedAt.Equal(*after.IntakeReleasedAt) {
			return domain.ErrReplacementStateInvalid
		}
	} else if after.IntakeReleasedAt != nil && ctx.Value(replacementIntakeReleaseKey{}) != q {
		return domain.ErrReplacementStateInvalid
	}
	if before.IntakeDrainRequestedAt != nil {
		if after.IntakeDrainRequestedAt == nil || !before.IntakeDrainRequestedAt.Equal(*after.IntakeDrainRequestedAt) {
			return domain.ErrReplacementStateInvalid
		}
	} else if after.IntakeDrainRequestedAt != nil && ctx.Value(replacementIntakeDrainKey{}) != q {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

// RecordReplacementIntakeDrain records an accepted queue kick, never provider
// delivery or reading. The controller and activated lease must still match.
func (s *Store) RecordReplacementIntakeDrain(ctx context.Context, id string, revision int64, at time.Time) (domain.OrchestratorReplacement, bool, error) {
	var result domain.OrchestratorReplacement
	applied := false
	if at.IsZero() {
		return result, false, domain.ErrReplacementStateInvalid
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
		if op.IntakeDrainRequestedAt != nil {
			applied = true
			return nil
		}
		if op.Revision != revision || op.Phase != domain.ReplacementNotifying || op.IntakeReleasedAt == nil || op.WriterActivation == nil {
			return nil
		}
		proof, ok := op.ExpectedSuccessorController()
		if !ok {
			return nil
		}
		targetRow, err := q.GetSession(tx, op.SuccessorSessionID)
		if err != nil {
			return err
		}
		target := rowToRecord(targetRow)
		if target.Kind != domain.KindOrchestrator || target.ProjectID != op.ProjectID || (target.ControllerOwner() != proof.Owner || target.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier) && !op.ActivatedSuccessorMatches(target) || target.HibernatedAt != nil || target.ProvisionState != domain.SessionProvisionReady {
			return nil
		}
		fenced, err := q.ReplacementWakeFenced(tx, target.ID)
		if err != nil || fenced {
			return err
		}
		now := at.UTC()
		op.IntakeDrainRequestedAt = &now
		op.UpdatedAt = now
		if op.Error != nil && op.Error.Code == "ORCHESTRATOR_HANDOFF_INTAKE_DRAIN_PENDING" {
			op.Error = nil
			op.ResumePhase = ""
		}
		saved, err := s.SaveOrchestratorReplacement(context.WithValue(tx, replacementIntakeDrainKey{}, q), op)
		if err != nil {
			return err
		}
		if !saved {
			return domain.ErrReplacementStateInvalid
		}
		op.Revision++
		result, applied = op, true
		return nil
	})
	return result, applied && err == nil, err
}
