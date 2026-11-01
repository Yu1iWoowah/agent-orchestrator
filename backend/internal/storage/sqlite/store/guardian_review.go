package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// RecordGuardianReview rechecks controller, policy and durable native ownership
// in one transaction. Observation failures never change an approval decision.
func (s *Store) RecordGuardianReview(ctx context.Context, scope domain.GuardianReviewScope, r domain.GuardianReview, now time.Time) error {
	if !r.InvalidMetadata && !r.Valid() {
		return errors.New("invalid guardian review")
	}
	session, generation := scope.SessionID, scope.Generation
	now = now.UTC()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "guardian review", func(q *gen.Queries) error {
		current, err := q.GetSession(ctx, session)
		if err != nil {
			return err
		}
		if current.ControllerGeneration != generation || current.SessionMode != "chat" {
			return nil
		}
		candidates, err := q.ListGuardianCandidates(ctx)
		if err != nil {
			return err
		}
		scoped := candidates[:0]
		for _, candidate := range candidates {
			if candidate.SessionID == session {
				scoped = append(scoped, candidate)
			}
		}
		owners, _, err := guardianOwners(ctx, q, scoped, now, true)
		if err != nil {
			return err
		}
		for _, o := range owners {
			if o.SessionID != session {
				continue
			}
			if o.AccountID != scope.AccountID || o.NativeRootID != scope.NativeRootID || scope.LaunchedAt.IsZero() || o.AccountBoundAt.After(scope.LaunchedAt) {
				return addGuardianAnomaly(ctx, q, session, domain.GuardianOwnerUnresolved, 1, now)
			}
			if r.InvalidMetadata {
				return addGuardianAnomaly(ctx, q, session, domain.GuardianInvalidMetadata, 1, now)
			}
			if o.OwnershipConflict {
				return addGuardianAnomaly(ctx, q, session, domain.GuardianOwnerConflict, 1, now)
			}
			if r.ParentThreadID != o.NativeRootID {
				// Only collector-proven child lineage is accepted; no cwd/time inference.
				if _, err := q.GetGuardianThread(ctx, gen.GetGuardianThreadParams{EpochID: o.EpochID, ThreadID: r.ParentThreadID}); errors.Is(err, sql.ErrNoRows) {
					return addGuardianAnomaly(ctx, q, session, domain.GuardianOwnerUnresolved, 1, now)
				} else if err != nil {
					return err
				}
			}
			at := r.StartedAt
			// A completion time alone cannot prove a newly created review.
			// In particular it must not revive an expired or purged review.

			if at == nil {
				return addGuardianAnomaly(ctx, q, session, domain.GuardianUnknownTime, 1, now)
			}
			if !o.AcceptsProviderTime(*at, now) {
				return nil
			}
			if r.CompletedAt != nil && r.CompletedAt.After(now) {
				return addGuardianAnomaly(ctx, q, session, domain.GuardianInvalidMetadata, 1, now)
			}
			r.EpochID = o.EpochID
			r.FirstSeenAt = now
			r.RetentionAt = *at
			old, err := q.GetGuardianReview(ctx, gen.GetGuardianReviewParams{EpochID: o.EpochID, ParentThreadID: r.ParentThreadID, ReviewID: r.ReviewID})
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				previous := guardianReview(old)
				r = mergeGuardianReview(previous, r)
				if reflect.DeepEqual(previous, r) {
					return nil
				}
			}
			return q.PutGuardianReview(ctx, gen.PutGuardianReviewParams{EpochID: r.EpochID, ParentThreadID: r.ParentThreadID, ReviewID: r.ReviewID, ParentTurnID: r.ParentTurnID, TargetItemID: nullableStringPtr(r.TargetItemID), Action: r.Action, Status: r.Status, DecisionSource: r.DecisionSource, StartedAt: guardianTime(r.StartedAt), CompletedAt: guardianTime(r.CompletedAt), FirstSeenAt: r.FirstSeenAt, RetentionAt: r.RetentionAt, SawStarted: guardianBool(r.SawStarted), SawCompleted: guardianBool(r.SawCompleted), Conflict: guardianBool(r.Conflict)})
		}
		return addGuardianAnomaly(ctx, q, session, domain.GuardianOwnerUnresolved, 1, now)
	})
}

func mergeGuardianReview(old, next domain.GuardianReview) domain.GuardianReview {
	sameTime := func(a, b *time.Time) bool { return a == nil || b == nil || a.Equal(*b) }
	if old.ParentTurnID != next.ParentTurnID || !reflect.DeepEqual(old.TargetItemID, next.TargetItemID) || old.Action != next.Action ||
		!sameTime(old.StartedAt, next.StartedAt) || !sameTime(old.CompletedAt, next.CompletedAt) ||
		(old.SawCompleted && next.SawCompleted && (old.Status != next.Status || old.DecisionSource != next.DecisionSource)) {
		old.Conflict = true
		return old
	}
	if old.StartedAt == nil {
		old.StartedAt = next.StartedAt
	}
	if old.CompletedAt == nil {
		old.CompletedAt = next.CompletedAt
	}
	if next.RetentionAt.Before(old.RetentionAt) {
		old.RetentionAt = next.RetentionAt
	}
	if next.SawCompleted && !old.SawCompleted {
		old.Status = next.Status
		old.DecisionSource = next.DecisionSource
	}
	old.SawStarted = old.SawStarted || next.SawStarted
	old.SawCompleted = old.SawCompleted || next.SawCompleted
	return old
}

func nullableStringPtr(v *string) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *v, Valid: true}
}
func guardianTime(v *time.Time) sql.NullTime {
	if v == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: v.UTC(), Valid: true}
}
func guardianTimePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	return &v.Time
}
func guardianReview(r gen.GuardianReview) domain.GuardianReview {
	var target *string
	if r.TargetItemID.Valid {
		target = &r.TargetItemID.String
	}
	return domain.GuardianReview{EpochID: r.EpochID, ParentThreadID: r.ParentThreadID, ReviewID: r.ReviewID, ParentTurnID: r.ParentTurnID, TargetItemID: target, Action: r.Action, Status: r.Status, DecisionSource: r.DecisionSource, StartedAt: guardianTimePtr(r.StartedAt), CompletedAt: guardianTimePtr(r.CompletedAt), FirstSeenAt: r.FirstSeenAt, RetentionAt: r.RetentionAt, SawStarted: r.SawStarted != 0, SawCompleted: r.SawCompleted != 0, Conflict: r.Conflict != 0}
}
