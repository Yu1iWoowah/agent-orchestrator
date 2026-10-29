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

type replacementSourceReleaseKey struct{}

func preserveReplacementSourceRelease(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	if before.SourceIntakeReleasedAt != nil {
		if ctx.Value(replacementResetKey{}) != q && !replacementTimesEqual(before.SourceIntakeReleasedAt, after.SourceIntakeReleasedAt) {
			return domain.ErrReplacementStateInvalid
		}
	} else if after.SourceIntakeReleasedAt != nil && ctx.Value(replacementSourceReleaseKey{}) != q {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

// ReleaseReplacementSourceIntake unfreezes only the unchanged source's writer
// and ordinary input. It neither resets evidence nor performs provider actions.
func (s *Store) ReleaseReplacementSourceIntake(ctx context.Context, id string, revision int64, at time.Time) (domain.OrchestratorReplacement, bool, error) {
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
		if op.SourceIntakeReleasedAt != nil {
			applied = true
			return nil
		}
		if op.Revision != revision || !op.CanReleaseSourceIntake() {
			return nil
		}
		sourceRow, err := q.GetSession(tx, op.SourceSessionID)
		if err != nil {
			return err
		}
		source := rowToRecord(sourceRow)
		if source.ProjectID != op.ProjectID || source.Kind != domain.KindOrchestrator || source.IsTerminated || source.ControllerOwner() != op.SourceOwner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier || source.HibernatedAt != nil || source.Activity.State == domain.ActivityExited {
			return nil
		}
		document, err := q.GetProjectDocumentState(tx, string(op.ProjectID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var state domain.ProjectDocumentState
			if err := json.Unmarshal([]byte(document.StateJson), &state); err != nil {
				return err
			}
			lease := state.Lease
			if lease != nil {
				if lease.SessionID != source.ID || lease.Owner != op.SourceOwner || (lease.ReplacementID != "" && lease.ReplacementID != op.ID) || (op.Preparation != nil && op.Preparation.LeaseEpoch > 0 && lease.Epoch != op.Preparation.LeaseEpoch) {
					return nil
				}
				if lease.Frozen {
					if lease.ReplacementID != op.ID {
						return nil
					}
					lease.Frozen = false
					state.Revision = document.Revision
					saved, err := s.SaveProjectDocumentState(context.WithValue(tx, projectDocumentPublicationKey{}, q), op.ProjectID, state)
					if err != nil {
						return err
					}
					if !saved {
						return domain.ErrReplacementStateInvalid
					}
				}
			} else if op.FrozenAt != nil || op.Preparation != nil && op.Preparation.LeaseEpoch > 0 {
				return nil
			}
		} else if op.FrozenAt != nil || op.Preparation != nil && op.Preparation.LeaseEpoch > 0 {
			return nil
		}
		now := at.UTC()
		op.SourceIntakeReleasedAt = &now
		op.UpdatedAt = now
		saved, err := s.SaveOrchestratorReplacement(context.WithValue(tx, replacementSourceReleaseKey{}, q), op)
		if err != nil {
			return err
		}
		if !saved {
			return domain.ErrReplacementStateInvalid
		}
		if err := restoreReplacementHumanProjection(tx, q, source.ID); err != nil {
			return err
		}
		op.Revision++
		result = op
		applied = true
		return nil
	})
	return result, applied && err == nil, err
}
