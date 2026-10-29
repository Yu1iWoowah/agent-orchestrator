package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ConversationAttention never acquires the provider writer lock. Counts, rows,
// read revisions and the branch identity all come from the same read snapshot.
func (s *Store) ConversationAttention(ctx context.Context, session domain.SessionID, conversationID string, query domain.ConversationAttentionQuery) (domain.ConversationAttentionPage, error) {
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.ConversationAttentionPage{}, fmt.Errorf("begin attention read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	page, err := s.conversationAttentionQueries(ctx, gen.New(tx), session, conversationID, query)
	if err != nil {
		return domain.ConversationAttentionPage{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.ConversationAttentionPage{}, fmt.Errorf("commit attention read: %w", err)
	}
	return page, nil
}

func (s *Store) conversationAttentionQueries(ctx context.Context, q *gen.Queries, session domain.SessionID, conversationID string, query domain.ConversationAttentionQuery) (domain.ConversationAttentionPage, error) {
	conversation, err := q.SelectConversationByID(ctx, conversationID)
	if err != nil {
		return domain.ConversationAttentionPage{}, err
	}
	if conversation.CurrentSessionID == nil || *conversation.CurrentSessionID != session {
		return domain.ConversationAttentionPage{}, domain.ErrNoConversation
	}
	floor, err := s.conversationVisibleAfterSequenceQueries(ctx, q, conversation)
	if err != nil {
		return domain.ConversationAttentionPage{}, err
	}
	if query.Limit <= 0 {
		query.Limit = 20
	}
	if query.Limit > 100 {
		query.Limit = 100
	}
	if query.BeforeSequence <= 0 {
		query.BeforeSequence = math.MaxInt64
	}
	if query.State == "" {
		query.State = "open"
	}
	counts, err := q.CountConversationAttention(ctx, gen.CountConversationAttentionParams{ConversationID: conversationID, VisibleAfterSequence: floor})
	if err != nil {
		return domain.ConversationAttentionPage{}, fmt.Errorf("count attention: %w", err)
	}
	rows, err := q.SelectConversationAttentionPage(ctx, gen.SelectConversationAttentionPageParams{
		ConversationID: conversationID, VisibleAfterSequence: floor,
		PageState: query.State, BeforeSequence: query.BeforeSequence,
		PageLimit: query.Limit + 1, ActivityID: query.ActivityID,
	})
	if err != nil {
		return domain.ConversationAttentionPage{}, fmt.Errorf("read attention: %w", err)
	}
	result := domain.ConversationAttentionPage{
		ConversationID: conversationID, ActiveBranchID: conversation.ActiveBranchID,
		Items:         make([]domain.ConversationAttentionItem, 0, len(rows)),
		Counts:        domain.ConversationAttentionCounts{Open: counts.OpenCount, Unread: counts.UnreadCount, History: counts.HistoryCount},
		HasMoreBefore: int64(len(rows)) > query.Limit,
	}
	if result.HasMoreBefore {
		rows = rows[:query.Limit]
		result.NextBeforeSequence = rows[len(rows)-1].ConversationActivity.Sequence
	}
	for _, row := range rows {
		result.Items = append(result.Items, domain.ConversationAttentionItem{
			Activity: activityToDomain(row.ConversationActivity), BranchID: row.ConversationActivity.BranchID,
			Actionable: row.Actionable != 0, Read: row.LastSeenRevision >= row.ConversationActivity.Revision,
		})
	}
	return result, nil
}

// MarkConversationAttentionRead is an explicit seen-revision command, never a
// provider decision. A stale branch, request or content revision cannot mark a
// replacement request seen; retrying the same revision is a no-op.
func (s *Store) MarkConversationAttentionRead(ctx context.Context, session domain.SessionID, conversationID, branchID, activityID string, revision int64, at time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "mark attention read", func(q *gen.Queries) error {
		page, err := s.conversationAttentionQueries(ctx, q, session, conversationID, domain.ConversationAttentionQuery{State: "all", ActivityID: activityID, Limit: 1})
		if err != nil {
			return err
		}
		if page.ActiveBranchID != branchID || len(page.Items) != 1 || !page.Items[0].Actionable || page.Items[0].Activity.Revision != revision {
			return domain.ErrAttentionChanged
		}
		return q.MarkConversationAttentionRead(ctx, gen.MarkConversationAttentionReadParams{ConversationID: conversationID, ActivityID: activityID, Revision: revision, SeenAt: at.UTC()})
	})
}
