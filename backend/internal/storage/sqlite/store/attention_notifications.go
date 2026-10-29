package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func attentionNotificationKey(item domain.ProjectAttentionItem) string {
	return fmt.Sprintf("attention:%s:%d", item.ID, item.Revision)
}

// HasCanonicalAttention lets lifecycle's generic needs-input hint yield to a stable source.
func (s *Store) HasCanonicalAttention(ctx context.Context, session domain.SessionID) (bool, error) {
	counts, err := s.qr.CountAttentionQuestions(ctx, string(session))
	if err != nil {
		return false, err
	}
	if counts.OpenCount > 0 {
		return true, nil
	}
	conversation, err := s.qr.SelectConversationBySession(ctx, &session)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	page, err := s.ConversationAttention(ctx, session, conversation.ID, domain.ConversationAttentionQuery{State: "open", Limit: 1})
	return page.Counts.Open > 0, err
}

// A receipt survives clearing history, so reconnect cannot replay an old revision.
// The fast receipt read avoids a writer lock for every unchanged poll.
func (s *Store) createAttentionNotification(ctx context.Context, rec domain.NotificationRecord) (domain.NotificationRecord, bool, error) {
	sent, err := s.qr.AttentionNotificationWasSent(ctx, rec.SourceKey)
	if err != nil || sent {
		return domain.NotificationRecord{}, false, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.NotificationRecord{}, false, err
	}
	defer s.writeMu.Unlock()
	var result domain.NotificationRecord
	inserted := false
	err = s.inTx(ctx, "persist attention notification", func(q *gen.Queries) error {
		sent, err := q.AttentionNotificationWasSent(ctx, rec.SourceKey)
		if err != nil || sent {
			return err
		}
		live, err := s.attentionNotificationCurrent(ctx, q, rec)
		if err != nil || !live {
			return err
		}
		existing, err := q.GetNotificationBySourceKey(ctx, rec.SourceKey)
		if err == nil {
			result = notificationFromGen(existing)
			return q.InsertAttentionNotificationReceipt(ctx, gen.InsertAttentionNotificationReceiptParams{SourceKey: rec.SourceKey, ProjectID: string(rec.ProjectID)})
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		row, err := q.CreateNotification(ctx, gen.CreateNotificationParams{ID: rec.ID, SessionID: rec.SessionID, ProjectID: optionalProjectID(rec.ProjectID), Type: rec.Type, Title: rec.Title, Body: rec.Body, Status: rec.Status, CreatedAt: rec.CreatedAt, SourceKey: rec.SourceKey})
		if err != nil {
			return err
		}
		if err := q.InsertAttentionNotificationReceipt(ctx, gen.InsertAttentionNotificationReceiptParams{SourceKey: rec.SourceKey, ProjectID: string(rec.ProjectID)}); err != nil {
			return err
		}
		result = notificationFromGen(row)
		inserted = true
		return nil
	})
	return result, inserted, err
}

func (s *Store) attentionNotificationCurrent(ctx context.Context, q *gen.Queries, rec domain.NotificationRecord) (bool, error) {
	source := strings.TrimPrefix(rec.SourceKey, "attention:")
	last := strings.LastIndex(source, ":")
	if last < 0 {
		return false, nil
	}
	revision, err := strconv.ParseInt(source[last+1:], 10, 64)
	if err != nil {
		return false, domain.ErrInvalidNotificationRecord
	}
	source = source[:last]
	if strings.HasPrefix(source, "question:") {
		row, err := q.GetAttentionQuestion(ctx, gen.GetAttentionQuestionParams{OriginSessionID: string(rec.SessionID), ID: strings.TrimPrefix(source, "question:")})
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return row.ProjectID.String == string(rec.ProjectID) && row.State == "open" && row.Version == revision && row.SeenVersion < revision && (!row.ExpiresAt.Valid || row.ExpiresAt.Time.After(rec.CreatedAt)), nil
	}
	if !strings.HasPrefix(source, "provider:") {
		return false, nil
	}
	owner, err := q.GetSession(ctx, rec.SessionID)
	if err != nil {
		return false, err
	}
	if owner.ProjectID == nil || *owner.ProjectID != rec.ProjectID {
		return false, nil
	}
	conversation, err := q.SelectConversationBySession(ctx, &rec.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	page, err := s.conversationAttentionQueries(ctx, q, rec.SessionID, conversation.ID, domain.ConversationAttentionQuery{State: "open", Limit: 1, ActivityID: strings.TrimPrefix(source, "provider:")})
	if err != nil {
		return false, err
	}
	return len(page.Items) == 1 && !page.Items[0].Read && page.Items[0].Activity.Revision == revision, nil
}

// ReconcileAttentionNotifications updates only canonical source facts; it never
// closes a question or changes U1 report delivery. No lock/write on an unchanged snapshot.
func (s *Store) ReconcileAttentionNotifications(ctx context.Context, project domain.ProjectID, items []domain.ProjectAttentionItem, at time.Time) ([]domain.NotificationRecord, error) {
	rows, err := s.qr.ListProjectAttentionNotifications(ctx, optionalProjectID(project))
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for _, item := range items {
		active[attentionNotificationKey(item)] = item.Read
	}
	changes := []gen.Notification{}
	for _, row := range rows {
		read, exists := active[row.SourceKey]
		if !exists || read && row.Status == domain.NotificationUnread {
			changes = append(changes, row)
		}
	}
	if len(changes) == 0 {
		return nil, nil
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer s.writeMu.Unlock()
	resolved := []domain.NotificationRecord{}
	err = s.inTx(ctx, "reconcile attention notifications", func(q *gen.Queries) error {
		for _, row := range changes {
			if _, exists := active[row.SourceKey]; exists {
				if err := q.ReadAttentionNotification(ctx, row.ID); err != nil {
					return err
				}
				continue
			}
			updated, err := q.ResolveAttentionNotification(ctx, gen.ResolveAttentionNotificationParams{ResolvedAt: nullTime(at), ID: row.ID})
			if err != nil {
				return err
			}
			if !updated.DismissedAt.Valid {
				resolved = append(resolved, notificationFromGen(updated))
			}
		}
		return nil
	})
	return resolved, err
}
