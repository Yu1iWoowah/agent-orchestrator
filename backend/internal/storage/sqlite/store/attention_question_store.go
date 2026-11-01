package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func questionFingerprint(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

// CreateAttentionQuestion atomically stores a validated question and its readable compatibility record.
func (s *Store) CreateAttentionQuestion(ctx context.Context, command domain.AttentionQuestionCreate, at time.Time) (domain.AttentionQuestionRecord, bool, error) {
	payload, err := domain.ParseAttentionQuestionPayload(command.Payload, command.Kind)
	if err != nil {
		return domain.AttentionQuestionRecord{}, false, err
	}
	if err := domain.ValidateAttentionClientKey(command.ClientKey); err != nil {
		return domain.AttentionQuestionRecord{}, false, err
	}
	if command.ID == "" || command.OriginSessionID == "" || command.Audience != "user" {
		return domain.AttentionQuestionRecord{}, false, &domain.AttentionValidationError{Field: "audience", Message: "question ID, origin session and user audience are required"}
	}
	if command.ExpiresAt != nil {
		utc := command.ExpiresAt.UTC()
		command.ExpiresAt = &utc
	}
	fingerprint, err := questionFingerprint(struct {
		Payload      domain.AttentionQuestionPayload
		Kind         domain.AttentionQuestionKind
		Audience     string
		ExpiresAt    *time.Time
		SupersedesID string
	}{payload, command.Kind, command.Audience, command.ExpiresAt, command.SupersedesID})
	if err != nil {
		return domain.AttentionQuestionRecord{}, false, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.AttentionQuestionRecord{}, false, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AttentionQuestionRecord{}, false, err
	}
	defer s.writeMu.Unlock()
	var result domain.AttentionQuestionRecord
	created := false
	err = s.inTx(ctx, "create attention question", func(q *gen.Queries) error {
		forwardedBy := command.OriginSessionID
		if command.SourceReportID != "" {
			var err error
			command.OriginSessionID, err = attentionReportOwner(ctx, q, forwardedBy, command.ActorVerifier, command.SourceReportID)
			if err != nil {
				return err
			}
			hash := sha256.Sum256([]byte(command.SourceReportID))
			command.ClientKey = "rpt_" + hex.EncodeToString(hash[:])[:60]
		}
		existing, err := q.FindAttentionQuestionKey(ctx, gen.FindAttentionQuestionKeyParams{OriginSessionID: string(command.OriginSessionID), ClientKey: command.ClientKey})
		if err == nil {
			if command.SourceReportID != "" {
				linked, err := q.GetAttentionReportQuestion(ctx, command.SourceReportID)
				if errors.Is(err, sql.ErrNoRows) || err == nil && linked != existing.ID {
					return domain.ErrAttentionQuestionConflict
				}
				if err != nil {
					return err
				}
			}
			if existing.CreateFingerprint != fingerprint {
				return domain.ErrAttentionQuestionConflict
			}
			result, err = questionRecord(ctx, q, existing)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if command.ExpiresAt != nil && !command.ExpiresAt.After(at) {
			return &domain.AttentionValidationError{Field: "expiresAt", Message: "must be in the future"}
		}
		owner, err := q.GetSession(ctx, command.OriginSessionID)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrAttentionQuestionNotFound
		}
		if err != nil {
			return err
		}
		if owner.IsTerminated {
			return domain.ErrAttentionQuestionConflict
		}
		if command.SupersedesID != "" {
			previous, err := questionRow(ctx, q, command.OriginSessionID, command.SupersedesID)
			if err != nil {
				return err
			}
			if previous.State == "open" {
				return domain.ErrAttentionQuestionConflict
			}
		}
		project := sql.NullString{}
		if owner.ProjectID != nil {
			project = nullableString(string(*owner.ProjectID))
		}
		row, err := q.InsertAttentionQuestion(ctx, gen.InsertAttentionQuestionParams{ID: command.ID, ProjectID: project, OriginSessionID: string(command.OriginSessionID), OriginHarness: string(owner.Harness), ClientKey: command.ClientKey, Audience: command.Audience, Kind: string(command.Kind), PayloadJson: string(raw), CreateFingerprint: fingerprint, SupersedesID: nullableString(command.SupersedesID), CreatedAt: at.UTC(), UpdatedAt: at.UTC(), ExpiresAt: questionNullTime(command.ExpiresAt)})
		if err != nil {
			return err
		}
		if command.SourceReportID != "" {
			if err := q.InsertAttentionReportQuestion(ctx, gen.InsertAttentionReportQuestionParams{ReportID: command.SourceReportID, QuestionID: row.ID, ForwardedBy: string(forwardedBy), CreatedAt: at.UTC()}); err != nil {
				return err
			}
		}
		result, err = questionRecord(ctx, q, row)
		if err != nil {
			return err
		}
		if err := appendQuestionCompatibility(ctx, q, result, "created", questionContext(result), at); err != nil {
			return err
		}
		created = true
		return nil
	})
	return result, created, err
}

func questionNullTime(value *time.Time) sql.NullTime {
	if value == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: value.UTC(), Valid: true}
}
func questionTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	at := value.Time.UTC()
	return &at
}

func questionRow(ctx context.Context, q *gen.Queries, session domain.SessionID, id string) (gen.AttentionQuestion, error) {
	row, err := q.GetAttentionQuestion(ctx, gen.GetAttentionQuestionParams{OriginSessionID: string(session), ID: id})
	if errors.Is(err, sql.ErrNoRows) {
		return row, domain.ErrAttentionQuestionNotFound
	}
	return row, err
}

func questionRecord(ctx context.Context, q *gen.Queries, row gen.AttentionQuestion) (domain.AttentionQuestionRecord, error) {
	result := domain.AttentionQuestionRecord{ID: row.ID, ProjectID: domain.ProjectID(row.ProjectID.String), OriginSessionID: domain.SessionID(row.OriginSessionID), OriginHarness: row.OriginHarness, Audience: row.Audience, Kind: domain.AttentionQuestionKind(row.Kind), ClientKey: row.ClientKey, State: row.State, Version: row.Version, Sequence: row.Sequence, SupersedesID: row.SupersedesID.String, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ExpiresAt: questionTime(row.ExpiresAt), ClosedAt: questionTime(row.ClosedAt), ClosedBy: row.ClosedBy, CloseReason: row.CloseReason, Read: row.SeenVersion >= row.Version}
	sourceReport, err := q.GetQuestionSourceReport(ctx, row.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	result.SourceReportID = sourceReport
	if err := json.Unmarshal([]byte(row.PayloadJson), &result.Payload); err != nil {
		return result, fmt.Errorf("decode question %s: %w", row.ID, err)
	}
	answer, err := q.GetAttentionAnswer(ctx, row.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	receipt, err := questionReceipt(answer)
	if err != nil {
		return result, err
	}
	result.Answer = &receipt
	return result, nil
}

// Legacy answers remain arrays; groups with notes use an envelope in the same JSON column.
type savedQuestionAnswers struct {
	Answers []domain.AttentionQuestionAnswer `json:"answers"`
	Notes   *string                          `json:"notes"`
}

func questionReceipt(row gen.AttentionAnswer) (domain.AttentionAnswerReceipt, error) {
	result := domain.AttentionAnswerReceipt{ClientAnswerID: row.ClientAnswerID, QuestionVersion: row.QuestionVersion, SavedAt: row.SavedAt, DeliveryState: row.DeliveryState, DeliveredAt: questionTime(row.DeliveredAt), LastError: row.LastError}
	var err error
	if strings.HasPrefix(strings.TrimSpace(row.AnswersJson), "[") {
		err = json.Unmarshal([]byte(row.AnswersJson), &result.Answers)
	} else {
		var saved savedQuestionAnswers
		err = json.Unmarshal([]byte(row.AnswersJson), &saved)
		result.Answers, result.Notes = saved.Answers, saved.Notes
	}
	return result, err
}

// GetAttentionQuestion reads a question and its answer in one read-only snapshot.
func (s *Store) GetAttentionQuestion(ctx context.Context, session domain.SessionID, id string) (domain.AttentionQuestionRecord, error) {
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.AttentionQuestionRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	q := gen.New(tx)
	row, err := questionRow(ctx, q, session, id)
	if err != nil {
		return domain.AttentionQuestionRecord{}, err
	}
	record, err := questionRecord(ctx, q, row)
	if err == nil {
		err = tx.Commit()
	}
	return record, err
}

// ExpireAttentionQuestions closes only open questions with due explicit deadlines.
func (s *Store) ExpireAttentionQuestions(ctx context.Context, session domain.SessionID, at time.Time) error {
	needed, err := s.qr.AttentionQuestionsNeedExpiration(ctx, gen.AttentionQuestionsNeedExpirationParams{OriginSessionID: string(session), At: sql.NullTime{Time: at.UTC(), Valid: true}})
	if err != nil || !needed {
		return err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	_, err = s.qw.ExpireAttentionQuestions(ctx, gen.ExpireAttentionQuestionsParams{OriginSessionID: string(session), At: sql.NullTime{Time: at.UTC(), Valid: true}})
	return err
}

// AttentionQuestionExpirationSessions lists sessions requiring a deadline sweep.
func (s *Store) AttentionQuestionExpirationSessions(ctx context.Context, at time.Time) ([]domain.SessionID, error) {
	rows, err := s.qr.AttentionQuestionExpirationSessions(ctx, sql.NullTime{Time: at.UTC(), Valid: true})
	if err != nil {
		return nil, err
	}
	result := make([]domain.SessionID, 0, len(rows))
	for _, id := range rows {
		result = append(result, domain.SessionID(id))
	}
	return result, nil
}

// ListAttentionQuestions reads lifecycle counts and a bounded page from one snapshot.
func (s *Store) ListAttentionQuestions(ctx context.Context, session domain.SessionID, query domain.AttentionQuestionQuery, at time.Time) (domain.AttentionQuestionPage, error) {
	result := domain.AttentionQuestionPage{Items: []domain.AttentionQuestionRecord{}}
	if query.State == "" {
		query.State = "open"
	}
	if query.State != "open" && query.State != "history" && query.State != "all" {
		return result, &domain.AttentionValidationError{Field: "state", Message: "must be open or history"}
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
	if err := s.ExpireAttentionQuestions(ctx, session, at); err != nil {
		return result, err
	}
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	q := gen.New(tx)
	counts, err := q.CountAttentionQuestions(ctx, string(session))
	if err != nil {
		return result, err
	}
	result.Open, result.Unread, result.History = counts.OpenCount, counts.UnreadCount, counts.HistoryCount
	rows, err := q.ListAttentionQuestions(ctx, gen.ListAttentionQuestionsParams{OriginSessionID: string(session), PageState: query.State, BeforeSequence: query.BeforeSequence, PageLimit: query.Limit + 1})
	if err != nil {
		return result, err
	}
	if int64(len(rows)) > query.Limit {
		result.HasMoreBefore = true
		rows = rows[:query.Limit]
		result.NextBeforeSequence = rows[len(rows)-1].Sequence
	}
	for _, row := range rows {
		record, err := questionRecord(ctx, q, row)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, record)
	}
	return result, tx.Commit()
}

func openQuestion(row gen.AttentionQuestion, version int64, at time.Time) error {
	if row.State != "open" || row.Version != version || (row.ExpiresAt.Valid && !row.ExpiresAt.Time.After(at)) {
		return domain.ErrAttentionQuestionConflict
	}
	return nil
}

// AnswerAttentionQuestion atomically saves the full group, terminal state and one delivery item.
func (s *Store) AnswerAttentionQuestion(ctx context.Context, session domain.SessionID, submission domain.AttentionAnswerSubmission, at time.Time) (domain.AttentionAnswerReceipt, error) {
	raw, err := json.Marshal(submission)
	if err != nil {
		return domain.AttentionAnswerReceipt{}, err
	}
	submission, err = domain.ParseAttentionAnswer(raw)
	if err != nil {
		return domain.AttentionAnswerReceipt{}, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AttentionAnswerReceipt{}, err
	}
	defer s.writeMu.Unlock()
	var receipt domain.AttentionAnswerReceipt
	err = s.inTx(ctx, "answer attention question", func(q *gen.Queries) error {
		row, err := questionRow(ctx, q, session, submission.QuestionID)
		if err != nil {
			return err
		}
		prior, priorErr := q.GetAttentionAnswer(ctx, row.ID)
		if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
			return priorErr
		}
		if priorErr == nil {
			if prior.ClientAnswerID != submission.ClientAnswerID || prior.QuestionVersion != submission.ExpectedVersion {
				return domain.ErrAttentionQuestionConflict
			}
		} else if err := openQuestion(row, submission.ExpectedVersion, at); err != nil {
			return err
		}
		var payload domain.AttentionQuestionPayload
		if err := json.Unmarshal([]byte(row.PayloadJson), &payload); err != nil {
			return err
		}
		answers, err := domain.CanonicalAttentionAnswers(payload, submission.Answers)
		if err != nil {
			if priorErr == nil {
				return domain.ErrAttentionQuestionConflict
			}
			return err
		}
		submission.Answers = answers
		fingerprint, err := questionFingerprint(submission)
		if err != nil {
			return err
		}
		if priorErr == nil {
			if prior.Fingerprint != fingerprint {
				return domain.ErrAttentionQuestionConflict
			}
			receipt, err = questionReceipt(prior)
			return err
		}
		question, err := questionRecord(ctx, q, row)
		if err != nil {
			return err
		}
		var saved any = answers
		if submission.Notes != nil {
			saved = savedQuestionAnswers{Answers: answers, Notes: submission.Notes}
		}
		answerJSON, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		message := questionAnswerContext(question, answers, submission.Notes)
		answer, err := q.InsertAttentionAnswer(ctx, gen.InsertAttentionAnswerParams{QuestionID: row.ID, ClientAnswerID: submission.ClientAnswerID, QuestionVersion: submission.ExpectedVersion, AnswersJson: string(answerJSON), Fingerprint: fingerprint, Message: message, SavedAt: at.UTC()})
		if err != nil {
			return err
		}
		changed, err := q.CloseAttentionQuestion(ctx, gen.CloseAttentionQuestionParams{ID: row.ID, OriginSessionID: string(session), ExpectedVersion: row.Version, State: "answered", Actor: "user", Reason: "answer_saved", At: sql.NullTime{Time: at.UTC(), Valid: true}})
		if err != nil {
			return err
		}
		if changed != 1 {
			return domain.ErrAttentionQuestionConflict
		}
		if err := appendQuestionCompatibility(ctx, q, question, "answered", message, at); err != nil {
			return err
		}
		receipt, err = questionReceipt(answer)
		return err
	})
	return receipt, err
}

// ReviseAttentionQuestion changes only an open matching version and resets its seen revision.
func (s *Store) ReviseAttentionQuestion(ctx context.Context, session domain.SessionID, id string, version int64, raw []byte, at time.Time) (domain.AttentionQuestionRecord, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AttentionQuestionRecord{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.AttentionQuestionRecord
	err := s.inTx(ctx, "revise attention question", func(q *gen.Queries) error {
		row, err := questionRow(ctx, q, session, id)
		if err != nil {
			return err
		}
		if err := openQuestion(row, version, at); err != nil {
			return err
		}
		payload, err := domain.ParseAttentionQuestionPayload(raw, domain.AttentionQuestionKind(row.Kind))
		if err != nil {
			return err
		}
		canonical, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		n, err := q.ReviseAttentionQuestion(ctx, gen.ReviseAttentionQuestionParams{ID: id, OriginSessionID: string(session), ExpectedVersion: version, PayloadJson: string(canonical), At: at.UTC()})
		if err != nil {
			return err
		}
		if n != 1 {
			return domain.ErrAttentionQuestionConflict
		}
		row, err = questionRow(ctx, q, session, id)
		if err != nil {
			return err
		}
		result, err = questionRecord(ctx, q, row)
		if err != nil {
			return err
		}
		return appendQuestionCompatibility(ctx, q, result, "revised", questionContext(result), at)
	})
	return result, err
}

// CancelAttentionQuestion closes only an open matching version without creating an answer.
func (s *Store) CancelAttentionQuestion(ctx context.Context, session domain.SessionID, id string, version int64, actor, reason string, at time.Time) (domain.AttentionQuestionRecord, error) {
	if actor != "user" && actor != "agent" {
		return domain.AttentionQuestionRecord{}, &domain.AttentionValidationError{Field: "actor", Message: "must be user or agent"}
	}
	if strings.TrimSpace(reason) == "" || utf8.RuneCountInString(reason) > 4000 {
		return domain.AttentionQuestionRecord{}, &domain.AttentionValidationError{Field: "reason", Message: "must contain 1–4000 Unicode code points"}
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AttentionQuestionRecord{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.AttentionQuestionRecord
	err := s.inTx(ctx, "cancel attention question", func(q *gen.Queries) error {
		row, err := questionRow(ctx, q, session, id)
		if err != nil {
			return err
		}
		if err := openQuestion(row, version, at); err != nil {
			return err
		}
		n, err := q.CloseAttentionQuestion(ctx, gen.CloseAttentionQuestionParams{ID: id, OriginSessionID: string(session), ExpectedVersion: version, State: "cancelled", Actor: actor, Reason: reason, At: sql.NullTime{Time: at.UTC(), Valid: true}})
		if err != nil {
			return err
		}
		if n != 1 {
			return domain.ErrAttentionQuestionConflict
		}
		row, err = questionRow(ctx, q, session, id)
		if err != nil {
			return err
		}
		result, err = questionRecord(ctx, q, row)
		if err != nil {
			return err
		}
		return appendQuestionCompatibility(ctx, q, result, "cancelled", fmt.Sprintf("Question %s cancelled by %s: %s", id, actor, reason), at)
	})
	return result, err
}

// MarkAttentionQuestionRead records a seen version independently of answer state.
func (s *Store) MarkAttentionQuestionRead(ctx context.Context, session domain.SessionID, id string, version int64, at time.Time) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	n, err := s.qw.ReadAttentionQuestion(ctx, gen.ReadAttentionQuestionParams{ID: id, OriginSessionID: string(session), ExpectedVersion: version, At: sql.NullTime{Time: at.UTC(), Valid: true}})
	if err == nil && n != 1 {
		return domain.ErrAttentionQuestionConflict
	}
	return err
}

// PendingAttentionAnswers returns queued answers ordered fairly across delivery attempts.
func (s *Store) PendingAttentionAnswers(ctx context.Context, limit int64) ([]domain.AttentionAnswerDelivery, error) {
	rows, err := s.qr.PendingAttentionAnswers(ctx, limit)
	if err != nil {
		return nil, err
	}
	result := make([]domain.AttentionAnswerDelivery, 0, len(rows))
	for _, row := range rows {
		result = append(result, domain.AttentionAnswerDelivery{QuestionID: row.AttentionAnswer.QuestionID, OriginSessionID: domain.SessionID(row.OriginSessionID), OriginHarness: row.OriginHarness, ClientMessageID: "question-answer-" + row.AttentionAnswer.QuestionID, Message: row.AttentionAnswer.Message})
	}
	return result, nil
}

// RecordAttentionAnswerDelivery acknowledges agent acceptance and human provenance atomically.
func (s *Store) RecordAttentionAnswerDelivery(ctx context.Context, id, state, lastError string, at time.Time) error {
	if state != "queued" && state != "delivered" && state != "failed" {
		return &domain.AttentionValidationError{Field: "deliveryState", Message: "invalid delivery state"}
	}
	delivered := sql.NullTime{}
	if state == "delivered" {
		delivered = sql.NullTime{Time: at.UTC(), Valid: true}
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "record attention answer delivery", func(q *gen.Queries) error {
		changed, err := q.RecordAttentionAnswerDelivery(ctx, gen.RecordAttentionAnswerDeliveryParams{QuestionID: id, DeliveryState: state, DeliveredAt: delivered, LastError: lastError})
		if err != nil {
			return err
		}
		if changed == 1 && state == "delivered" {
			return q.RecordAttentionAnswerHumanDirection(ctx, id)
		}
		return nil
	})
}

// Validate every handoff inside the same writer transaction as its unique link.
func attentionReportOwner(ctx context.Context, q *gen.Queries, caller domain.SessionID, verifier, id string) (domain.SessionID, error) {
	report, err := q.GetReport(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.ErrAttentionQuestionNotFound
	}
	if err != nil {
		return "", err
	}
	project := domain.ProjectID(report.ProjectID)
	active, err := q.ActiveAttentionOrchestrator(ctx, &project)
	if errors.Is(err, sql.ErrNoRows) || err == nil && active != caller {
		return "", domain.ErrAttentionQuestionForbidden
	}
	if err != nil {
		return "", err
	}
	actor, err := q.GetSession(ctx, caller)
	if err != nil {
		return "", err
	}
	if verifier == "" || actor.BrowserCapabilityVerifier != verifier {
		return "", domain.ErrAttentionQuestionForbidden
	}
	worker, err := q.GetSession(ctx, domain.SessionID(report.SessionID))
	if err != nil {
		return "", err
	}
	if worker.Kind != domain.KindWorker || worker.ProjectID == nil || string(*worker.ProjectID) != report.ProjectID || worker.IsTerminated || (report.State != "needs_input" && report.State != "stuck") {
		return "", domain.ErrAttentionQuestionForbidden
	}
	return domain.SessionID(report.SessionID), nil
}
