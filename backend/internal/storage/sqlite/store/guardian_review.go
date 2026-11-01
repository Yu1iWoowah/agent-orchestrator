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

// RecordGuardianReview applies a validated observation. A queued observation is
// removed only in the same transaction that commits its retained fact.
func (s *Store) RecordGuardianReview(ctx context.Context, scope domain.GuardianReviewScope, r domain.GuardianReview, now time.Time) error {
	_, err := s.recordGuardianReview(ctx, scope, r, now, false)
	return err
}

// QueueGuardianReview participates in the archive/projection transaction. Only
// the metadata whitelist is queued; a committed activity cannot lose this fact.
func (s *Store) QueueGuardianReview(ctx context.Context, scope domain.GuardianReviewScope, r domain.GuardianReview, now time.Time) (bool, error) {
	return s.recordGuardianReview(ctx, scope, r, now, true)
}

func (s *Store) recordGuardianReview(ctx context.Context, scope domain.GuardianReviewScope, r domain.GuardianReview, now time.Time, pending bool) (bool, error) {
	enabled, err := s.conversationReader(ctx).GuardianSessionEnabled(ctx, scope.SessionID)
	if err != nil || !enabled {
		return false, err
	}
	if !r.InvalidMetadata && !r.Valid() {
		return false, errors.New("invalid guardian review")
	}
	now = now.UTC()
	// Archive callbacks already own the writer and pass its transaction in ctx.
	if _, ok := ctx.Value(conversationProjectionTxKey{}).(*gen.Queries); !ok {
		if err := s.writeMu.LockContext(ctx); err != nil {
			return false, err
		}
		defer s.writeMu.Unlock()
	}
	session := scope.SessionID
	recorded := false
	err = s.inTx(ctx, "guardian review", func(q *gen.Queries) error {
		enabled, err := q.GuardianSessionEnabled(ctx, scope.SessionID)
		if err != nil || !enabled {
			return err
		}
		current, err := q.GetSession(ctx, session)
		if err != nil {
			return err
		}
		if current.ControllerGeneration != scope.Generation || current.SessionMode != "chat" {
			return nil
		}
		candidates, err := q.ListGuardianReviewCandidates(ctx, session)
		if err != nil {
			return err
		}
		scoped := make([]gen.ListGuardianCandidatesRow, len(candidates))
		for i, candidate := range candidates {
			scoped[i] = gen.ListGuardianCandidatesRow(candidate)
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
				// Collector-proven G.parent=S establishes S's lineage to this root.
				proven, err := q.HasGuardianParent(ctx, gen.HasGuardianParentParams{EpochID: o.EpochID, ParentID: r.ParentThreadID})
				if err != nil {
					return err
				}
				if !proven {
					return addGuardianAnomaly(ctx, q, session, domain.GuardianOwnerUnresolved, 1, now)
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
			err := s.putGuardianReview(ctx, q, r, pending)
			recorded = err == nil
			return err
		}
		return addGuardianAnomaly(ctx, q, session, domain.GuardianOwnerUnresolved, 1, now)
	})
	return recorded, err
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
	old.Conflict = old.Conflict || next.Conflict
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

// putGuardianReview merges pending and retained facts without refreshing their
// first observation time. Pending removal and retained insertion are atomic.
func (s *Store) putGuardianReview(ctx context.Context, q *gen.Queries, r domain.GuardianReview, pending bool) error {
	key := gen.GetPendingGuardianReviewParams{EpochID: r.EpochID, ParentThreadID: r.ParentThreadID, ReviewID: r.ReviewID}
	queued, err := q.GetPendingGuardianReview(ctx, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		r = mergeGuardianReview(pendingGuardianReview(queued), r)
	}
	params := gen.PutGuardianReviewParams{EpochID: r.EpochID, ParentThreadID: r.ParentThreadID, ReviewID: r.ReviewID, ParentTurnID: r.ParentTurnID, TargetItemID: nullableStringPtr(r.TargetItemID), Action: r.Action, Status: r.Status, DecisionSource: r.DecisionSource, StartedAt: guardianTime(r.StartedAt), CompletedAt: guardianTime(r.CompletedAt), FirstSeenAt: r.FirstSeenAt, RetentionAt: r.RetentionAt, SawStarted: guardianBool(r.SawStarted), SawCompleted: guardianBool(r.SawCompleted), Conflict: guardianBool(r.Conflict)}
	if pending {
		return q.PutPendingGuardianReview(ctx, gen.PutPendingGuardianReviewParams(params))
	}
	old, err := q.GetGuardianReview(ctx, gen.GetGuardianReviewParams(key))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		previous := guardianReview(old)
		r = mergeGuardianReview(previous, r)
		if reflect.DeepEqual(previous, r) {
			return q.DeletePendingGuardianReview(ctx, gen.DeletePendingGuardianReviewParams(key))
		}
		params.Status = r.Status
		params.DecisionSource = r.DecisionSource
		params.StartedAt = guardianTime(r.StartedAt)
		params.CompletedAt = guardianTime(r.CompletedAt)
		params.RetentionAt = r.RetentionAt
		params.SawStarted = guardianBool(r.SawStarted)
		params.SawCompleted = guardianBool(r.SawCompleted)
		params.Conflict = guardianBool(r.Conflict)
	}
	if err := q.PutGuardianReview(ctx, params); err != nil {
		return err
	}
	return q.DeletePendingGuardianReview(ctx, gen.DeletePendingGuardianReviewParams(key))
}

func pendingGuardianReview(r gen.GuardianReviewPending) domain.GuardianReview {
	return guardianReview(gen.GuardianReview{EpochID: r.EpochID, ParentThreadID: r.ParentThreadID, ReviewID: r.ReviewID, ParentTurnID: r.ParentTurnID, TargetItemID: r.TargetItemID, Action: r.Action, Status: r.Status, DecisionSource: r.DecisionSource, StartedAt: r.StartedAt, CompletedAt: r.CompletedAt, FirstSeenAt: r.FirstSeenAt, RetentionAt: r.RetentionAt, SawStarted: r.SawStarted, SawCompleted: r.SawCompleted, Conflict: r.Conflict})
}

// ReplayGuardianReviews recovers already validated observations, including after
// a controller generation changes. Epoch deletion (purge) cascades to pending.
func (s *Store) ReplayGuardianReviews(ctx context.Context, now time.Time) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "guardian review recovery", func(q *gen.Queries) error {
		rows, err := q.ListPendingGuardianReviews(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if !row.RetentionAt.After(now.Add(-domain.GuardianRetention)) {
				if err := q.DeletePendingGuardianReview(ctx, gen.DeletePendingGuardianReviewParams{EpochID: row.EpochID, ParentThreadID: row.ParentThreadID, ReviewID: row.ReviewID}); err != nil {
					return err
				}
				continue
			}
			if err := s.putGuardianReview(ctx, q, pendingGuardianReview(row), false); err != nil {
				return err
			}
		}
		return nil
	})
}
