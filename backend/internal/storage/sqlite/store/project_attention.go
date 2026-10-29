package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type attentionCursor struct {
	At time.Time
	ID string
}

// ProjectAttention derives counts and source identity from one read-only snapshot.
func (s *Store) ProjectAttention(ctx context.Context, project domain.ProjectID, query domain.ProjectAttentionQuery, at time.Time) (domain.ProjectAttentionPage, error) {
	page := domain.ProjectAttentionPage{Items: []domain.ProjectAttentionItem{}}
	if query.Audience == "" {
		query.Audience = "user"
	}
	if query.Audience != "user" && query.Audience != "orchestrator" {
		return page, &domain.AttentionValidationError{Field: "audience", Message: "must be user or orchestrator"}
	}
	if query.Limit == 0 {
		query.Limit = 20
	}
	if query.Limit < 1 || query.Limit > 100 {
		return page, &domain.AttentionValidationError{Field: "limit", Message: "must be between 1 and 100"}
	}
	var cursor attentionCursor
	if query.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil || len(raw) > 1024 || json.Unmarshal(raw, &cursor) != nil || cursor.ID == "" || cursor.At.IsZero() {
			return page, &domain.AttentionValidationError{Field: "cursor", Message: "invalid cursor"}
		}
	}
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer func() { _ = tx.Rollback() }()
	q := gen.New(tx)
	if _, err = q.GetProject(ctx, project); errors.Is(err, sql.ErrNoRows) {
		return page, domain.ErrAttentionQuestionNotFound
	} else if err != nil {
		return page, err
	}
	sessions, err := q.ListSessionsByProject(ctx, &project)
	if err != nil {
		return page, err
	}
	harnesses := map[domain.SessionID]string{}
	names := map[domain.SessionID]string{}
	for _, session := range sessions {
		names[session.ID] = session.DisplayName
		harnesses[session.ID] = string(session.Harness)
	}
	all := []domain.ProjectAttentionItem{}
	if query.Audience == "orchestrator" {
		rows, err := q.ProjectAttentionReportSources(ctx, string(project))
		if err != nil {
			return page, err
		}
		for _, row := range rows {
			all = append(all, domain.ProjectAttentionItem{ID: "report:" + row.ID, Kind: "worker_report", SessionID: domain.SessionID(row.SessionID), SessionName: names[domain.SessionID(row.SessionID)], Title: row.Note, Revision: 1, CreatedAt: row.CreatedAt, Read: row.IsRead != 0, OriginHarness: harnesses[domain.SessionID(row.SessionID)], ReportID: row.ID, ReportState: row.State, DeliveryState: row.DeliveryState, AnswerState: "unknown", QuestionID: row.QuestionID})
		}
	} else {
		rows, err := q.ProjectAttentionQuestionSources(ctx, gen.ProjectAttentionQuestionSourcesParams{ProjectID: sql.NullString{String: string(project), Valid: true}, At: sql.NullTime{Time: at.UTC(), Valid: true}})
		if err != nil {
			return page, err
		}
		for _, row := range rows {
			all = append(all, domain.ProjectAttentionItem{ID: "question:" + row.ID, Kind: "agent_question", SessionID: domain.SessionID(row.OriginSessionID), SessionName: names[domain.SessionID(row.OriginSessionID)], Title: row.Title, Revision: row.Version, CreatedAt: row.CreatedAt, Read: row.SeenVersion >= row.Version, QuestionID: row.ID})
		}
		for _, session := range sessions {
			conversation, err := q.SelectConversationBySession(ctx, &session.ID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return page, err
			}
			// Iterate the bounded provider index without assuming its first page is the total.
			before := int64(0)
			for {
				sources, err := s.conversationAttentionQueries(ctx, q, session.ID, conversation.ID, domain.ConversationAttentionQuery{State: "open", BeforeSequence: before, Limit: 100})
				if err != nil {
					return page, err
				}
				for _, source := range sources.Items {
					kind := "provider_input"
					if source.Activity.Kind == domain.ActivityKindApproval {
						kind = "provider_approval"
					}
					all = append(all, domain.ProjectAttentionItem{ID: "provider:" + source.Activity.ID, Kind: kind, SessionID: session.ID, SessionName: session.DisplayName, Title: source.Activity.Summary, Revision: source.Activity.Revision, CreatedAt: source.Activity.CreatedAt, Read: source.Read, ConversationID: conversation.ID, ActivityID: source.Activity.ID, BranchID: sources.ActiveBranchID, SourceBranchID: source.BranchID, RequestID: source.Activity.RequestID})
				}
				if !sources.HasMoreBefore {
					break
				}
				before = sources.NextBeforeSequence
			}
		}
	}
	page.Open = int64(len(all))
	for _, item := range all {
		if !item.Read {
			page.Unread++
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].ID > all[j].ID
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	for _, item := range all {
		if query.Cursor != "" && (item.CreatedAt.After(cursor.At) || item.CreatedAt.Equal(cursor.At) && item.ID >= cursor.ID) {
			continue
		}
		if len(page.Items) == query.Limit {
			last := page.Items[len(page.Items)-1]
			raw, _ := json.Marshal(attentionCursor{At: last.CreatedAt, ID: last.ID})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		page.Items = append(page.Items, item)
	}
	return page, tx.Commit()
}

// ReadProjectAttentionReport records inspection, independently of U1 delivery and any answer.
func (s *Store) ReadProjectAttentionReport(ctx context.Context, project domain.ProjectID, id string, at time.Time) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "read project report", func(q *gen.Queries) error {
		row, err := q.GetReport(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || err == nil && row.ProjectID != string(project) {
			return domain.ErrAttentionQuestionNotFound
		}
		if err != nil {
			return err
		}
		return q.ReadAttentionReport(ctx, gen.ReadAttentionReportParams{ReportID: id, SeenAt: at.UTC()})
	})
}
