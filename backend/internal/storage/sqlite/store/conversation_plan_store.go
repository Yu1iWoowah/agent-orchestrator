package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ConversationPlans reads bounded structured projections on one consistent
// active lineage. Empty steps are a clear, not a reason to return an older plan.
func (s *Store) ConversationPlans(ctx context.Context, conversationID string, beforeSequence, limit int64) ([]domain.ConversationPlanVersion, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 101 {
		limit = 101
	}
	if beforeSequence <= 0 {
		beforeSequence = math.MaxInt64
	}
	if err := s.backfillConversationPlans(ctx, conversationID); err != nil {
		return nil, err
	}
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin plan read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := gen.New(tx)
	conversation, err := q.SelectConversationByID(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("read plan conversation: %w", err)
	}
	floor, err := s.conversationVisibleAfterSequenceQueries(ctx, q, conversation)
	if err != nil {
		return nil, err
	}
	activities, err := q.SelectConversationPlanActivities(ctx, gen.SelectConversationPlanActivitiesParams{
		ConversationID: conversationID, BeforeSequence: beforeSequence,
		VisibleAfterSequence: floor, PageLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("read plan activities: %w", err)
	}
	legacy, err := q.SelectLegacyConversationPlans(ctx, gen.SelectLegacyConversationPlansParams{
		ConversationID: conversationID, BeforeSequence: beforeSequence,
		VisibleAfterSequence: floor, PageLimit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("read legacy plans: %w", err)
	}
	versions, err := q.SelectConversationPlanVersions(ctx, gen.SelectConversationPlanVersionsParams{ConversationID: conversationID, BeforeSequence: beforeSequence, VisibleAfterSequence: floor, PageLimit: limit})
	if err != nil {
		return nil, fmt.Errorf("read projected plans: %w", err)
	}
	plans := make([]domain.ConversationPlanVersion, 0, len(activities)+len(legacy)+len(versions))
	appendPlan := func(version domain.ConversationPlanVersion, raw string) error {
		if err := json.Unmarshal([]byte(raw), &version.Plan); err != nil {
			return fmt.Errorf("decode plan at sequence %d: %w", version.Sequence, err)
		}
		plans = append(plans, version)
		return nil
	}
	for _, row := range versions {
		if strings.HasPrefix(row.SourceActivityID, "legacy-turn-") {
			row.SourceActivityID = ""
		}
		if err := appendPlan(domain.ConversationPlanVersion{ConversationID: conversationID, BranchID: row.BranchID, TurnID: row.TurnID, SourceActivityID: row.SourceActivityID, Sequence: row.AcceptedSequence, Revision: row.Revision, UpdatedAt: row.UpdatedAt}, row.PlanJson); err != nil {
			return nil, err
		}
	}
	for _, row := range activities {
		if err := appendPlan(domain.ConversationPlanVersion{
			ConversationID: conversationID, BranchID: row.BranchID, TurnID: row.TurnID,
			SourceActivityID: row.SourceActivityID, Sequence: row.Sequence,
			Revision: row.Revision, UpdatedAt: row.UpdatedAt,
		}, row.PlanJson); err != nil {
			return nil, err
		}
	}
	for _, row := range legacy {
		if err := appendPlan(domain.ConversationPlanVersion{
			ConversationID: conversationID, BranchID: row.BranchID, TurnID: row.TurnID,
			Sequence: row.Sequence, Revision: 1, UpdatedAt: row.UpdatedAt,
		}, row.PlanJson); err != nil {
			return nil, err
		}
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Sequence > plans[j].Sequence })
	if int64(len(plans)) > limit {
		plans = plans[:limit]
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit plan read: %w", err)
	}
	return plans, nil
}

// projectPlanActivity is called with the same transaction as the turn/activity
// write. Sequence is acceptance order, independent of a timeline row's position.
func projectPlanActivity(ctx context.Context, q *gen.Queries, conversationID, activityID string, now time.Time) error {
	sequence, err := q.NextConversationSequence(ctx, gen.NextConversationSequenceParams{ID: conversationID, UpdatedAt: now})
	if err != nil {
		return err
	}
	if err := q.ProjectConversationPlan(ctx, gen.ProjectConversationPlanParams{ConversationID: conversationID, ActivityID: activityID, AcceptedSequence: sequence, UpdatedAt: now}); err != nil {
		return err
	}
	return q.ArchiveConversationPlanHead(ctx, gen.ArchiveConversationPlanHeadParams{ConversationID: conversationID, AcceptedSequence: sequence})
}

// Backfill at most 100 legacy sources per read. It only inserts missing read
// projections; existing provider facts and accepted heads are never overwritten.
func (s *Store) backfillConversationPlans(ctx context.Context, conversationID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "backfill conversation plans", func(q *gen.Queries) error {
		activities, err := q.SelectConversationPlanActivities(ctx, gen.SelectConversationPlanActivitiesParams{ConversationID: conversationID, BeforeSequence: math.MaxInt64, PageLimit: 100})
		if err != nil {
			return err
		}
		for _, a := range activities {
			if err := q.BackfillLegacyPlanVersion(ctx, a.SourceActivityID); err != nil {
				return err
			}
		}
		turns, err := q.SelectLegacyConversationPlans(ctx, gen.SelectLegacyConversationPlansParams{ConversationID: conversationID, BeforeSequence: math.MaxInt64, PageLimit: int64(100 - len(activities))})
		if err != nil {
			return err
		}
		for _, t := range turns {
			if err := q.InsertLegacyTurnPlanVersion(ctx, t.TurnID); err != nil {
				return err
			}
		}
		return q.BackfillMissingConversationPlanHeads(ctx, conversationID)
	})
}
