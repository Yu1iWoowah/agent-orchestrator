package questions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Store is the persistence boundary for question state and delivery receipts.
type Store interface {
	ProjectAttention(context.Context, domain.ProjectID, domain.ProjectAttentionQuery, time.Time) (domain.ProjectAttentionPage, error)
	ReadProjectAttentionReport(context.Context, domain.ProjectID, string, time.Time) error
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	CreateAttentionQuestion(context.Context, domain.AttentionQuestionCreate, time.Time) (domain.AttentionQuestionRecord, bool, error)
	GetAttentionQuestion(context.Context, domain.SessionID, string) (domain.AttentionQuestionRecord, error)
	ListAttentionQuestions(context.Context, domain.SessionID, domain.AttentionQuestionQuery, time.Time) (domain.AttentionQuestionPage, error)
	ExpireAttentionQuestions(context.Context, domain.SessionID, time.Time) error
	AttentionQuestionExpirationSessions(context.Context, time.Time) ([]domain.SessionID, error)
	AnswerAttentionQuestion(context.Context, domain.SessionID, domain.AttentionAnswerSubmission, time.Time) (domain.AttentionAnswerReceipt, error)
	ReviseAttentionQuestion(context.Context, domain.SessionID, string, int64, []byte, time.Time) (domain.AttentionQuestionRecord, error)
	CancelAttentionQuestion(context.Context, domain.SessionID, string, int64, string, string, time.Time) (domain.AttentionQuestionRecord, error)
	MarkAttentionQuestionRead(context.Context, domain.SessionID, string, int64, time.Time) error
	PendingAttentionAnswers(context.Context, int64) ([]domain.AttentionAnswerDelivery, error)
	RecordAttentionAnswerDelivery(context.Context, string, string, string, time.Time) error
}

// Delivery acknowledges a stable message ID at the original agent acceptance boundary.
type Delivery interface {
	Submit(context.Context, domain.SessionID, string, string) error
}

// AttentionNotifications produces stable user-source notifications through the existing channel.
type AttentionNotifications interface {
	ReconcileAttention(context.Context, domain.ProjectID, []domain.ProjectAttentionItem) error
}

// Deps supplies persistence, semantic delivery and deterministic identity sources.
type Deps struct {
	Notifications AttentionNotifications
	Store         Store
	Delivery      Delivery
	Now           func() time.Time
	NewID         func() string
}

// Service coordinates durable questions, expiry and answer delivery retries.
type Service struct {
	notifications AttentionNotifications
	store         Store
	delivery      Delivery
	now           func() time.Time
	newID         func() string
	wake          chan struct{}
	deliveryMu    sync.Mutex
	retries       map[string]answerRetry
}

type answerRetry struct {
	session domain.SessionID
	delay   time.Duration
	at      time.Time
}

// New creates a question coordinator without starting background work.
func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.NewID == nil {
		deps.NewID = func() string { return "ask_" + uuid.NewString() }
	}
	return &Service{notifications: deps.Notifications, store: deps.Store, delivery: deps.Delivery, now: deps.Now, newID: deps.NewID, wake: make(chan struct{}, 1), retries: make(map[string]answerRetry)}
}

// Create creates or finds a question using its source-scoped retry key.
func (s *Service) Create(ctx context.Context, input domain.AttentionQuestionCreate) (domain.AttentionQuestionRecord, bool, error) {
	input.ID = s.newID()
	return s.store.CreateAttentionQuestion(ctx, input, s.now().UTC())
}

// Answer saves a complete answer group and wakes asynchronous delivery.
func (s *Service) Answer(ctx context.Context, session domain.SessionID, input domain.AttentionAnswerSubmission) (domain.AttentionAnswerReceipt, error) {
	receipt, err := s.store.AnswerAttentionQuestion(ctx, session, input, s.now().UTC())
	if err == nil {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return receipt, err
}

// Get expires due questions before reading the current source.
func (s *Service) Get(ctx context.Context, session domain.SessionID, id string) (domain.AttentionQuestionRecord, error) {
	if err := s.store.ExpireAttentionQuestions(ctx, session, s.now().UTC()); err != nil {
		return domain.AttentionQuestionRecord{}, err
	}
	return s.store.GetAttentionQuestion(ctx, session, id)
}

// List returns lifecycle counts and a bounded page for an existing session.
func (s *Service) List(ctx context.Context, session domain.SessionID, query domain.AttentionQuestionQuery) (domain.AttentionQuestionPage, error) {
	_, found, err := s.store.GetSession(ctx, session)
	if err != nil {
		return domain.AttentionQuestionPage{}, err
	}
	if !found {
		return domain.AttentionQuestionPage{}, domain.ErrAttentionQuestionNotFound
	}
	return s.store.ListAttentionQuestions(ctx, session, query, s.now().UTC())
}

// Revise replaces an open matching version without carrying prior consent.
func (s *Service) Revise(ctx context.Context, session domain.SessionID, id string, version int64, payload []byte) (domain.AttentionQuestionRecord, error) {
	return s.store.ReviseAttentionQuestion(ctx, session, id, version, payload, s.now().UTC())
}

// Cancel closes an open matching version without creating an answer.
func (s *Service) Cancel(ctx context.Context, session domain.SessionID, id string, version int64, actor, reason string) (domain.AttentionQuestionRecord, error) {
	return s.store.CancelAttentionQuestion(ctx, session, id, version, actor, reason, s.now().UTC())
}

func (s *Service) Read(ctx context.Context, session domain.SessionID, id string, version int64) error {
	return s.store.MarkAttentionQuestionRead(ctx, session, id, version, s.now().UTC())
}

// DeliverPending owns retries; saving an answer never waits for a provider.
// Submit must acknowledge the stable ID at the semantic acceptance boundary.
func (s *Service) DeliverPending(ctx context.Context) error {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	due, err := s.store.AttentionQuestionExpirationSessions(ctx, s.now().UTC())
	if err != nil {
		return err
	}
	for _, session := range due {
		if err := s.store.ExpireAttentionQuestions(ctx, session, s.now().UTC()); err != nil {
			return err
		}
	}
	if s.delivery == nil {
		return nil
	}
	items, err := s.store.PendingAttentionAnswers(ctx, 20)
	if err != nil {
		return err
	}
	// Native TUI acceptance can finish an outbox outside this coordinator.
	// Inspect retained retries independently of the bounded queued page.
	for id, retry := range s.retries {
		question, err := s.store.GetAttentionQuestion(ctx, retry.session, id)
		if errors.Is(err, domain.ErrAttentionQuestionNotFound) || err == nil && question.Answer != nil && question.Answer.DeliveryState != "queued" {
			delete(s.retries, id)
		} else if err != nil {
			return err
		}
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if retry := s.retries[item.QuestionID]; s.now().Before(retry.at) {
			continue
		}
		owner, found, err := s.store.GetSession(ctx, item.OriginSessionID)
		if err != nil {
			return err
		}
		if !found || owner.IsTerminated || string(owner.Harness) != item.OriginHarness {
			if err := s.store.RecordAttentionAnswerDelivery(ctx, item.QuestionID, "failed", "Origin session or agent is no longer available; the answer remains saved", s.now().UTC()); err != nil {
				return err
			}
			delete(s.retries, item.QuestionID)
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
		sendErr := s.delivery.Submit(attempt, item.OriginSessionID, item.Message, item.ClientMessageID)
		cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		state, lastError := "delivered", ""
		if sendErr != nil {
			state = "queued"
			lastError = sendErr.Error()
			if len(lastError) > 2000 {
				lastError = lastError[:2000]
			}
		}
		if err := s.store.RecordAttentionAnswerDelivery(ctx, item.QuestionID, state, lastError, s.now().UTC()); err != nil {
			return err
		}
		if sendErr == nil {
			delete(s.retries, item.QuestionID)
		} else {
			delay := s.retries[item.QuestionID].delay * 2
			if delay < 5*time.Second {
				delay = 5 * time.Second
			}
			if delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
			s.retries[item.QuestionID] = answerRetry{session: item.OriginSessionID, delay: delay, at: s.now().UTC().Add(delay)}
		}
	}
	return nil
}

// Start restores persisted work immediately, then wakes for new answers or
// retry deadlines. The caller cancels ctx and joins done during daemon shutdown.
func (s *Service) Start(ctx context.Context, onError func(error)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := s.RefreshNotifications(ctx); err != nil && onError != nil {
				onError(fmt.Errorf("attention notifications: %w", err))
			}
			if err := s.DeliverPending(ctx); err != nil && !errors.Is(err, context.Canceled) && onError != nil {
				onError(fmt.Errorf("deliver question answers: %w", err))
			}
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-ticker.C:
			}
		}
	}()
	return done
}

// ProjectList never treats report delivery as an answer or a user question.
func (s *Service) ProjectList(ctx context.Context, project domain.ProjectID, query domain.ProjectAttentionQuery) (domain.ProjectAttentionPage, error) {
	return s.store.ProjectAttention(ctx, project, query, s.now().UTC())
}

// ReadReport marks an inspected report seen without closing it.
func (s *Service) ReadReport(ctx context.Context, project domain.ProjectID, id string) error {
	return s.store.ReadProjectAttentionReport(ctx, project, id, s.now().UTC())
}

// RefreshNotifications restores stable sources after restart; failed reads never
// become an empty set that could resolve unseen pending decisions.
func (s *Service) RefreshNotifications(ctx context.Context) error {
	if s.notifications == nil {
		return nil
	}
	projects, ok := s.store.(interface {
		ListProjects(context.Context) ([]domain.ProjectRecord, error)
	})
	if !ok {
		return errors.New("project reader unavailable")
	}
	rows, err := projects.ListProjects(ctx)
	if err != nil {
		return err
	}
	for _, project := range rows {
		query := domain.ProjectAttentionQuery{Audience: "user", Limit: 100}
		items := []domain.ProjectAttentionItem{}
		for {
			page, err := s.store.ProjectAttention(ctx, domain.ProjectID(project.ID), query, s.now().UTC())
			if err != nil {
				return err
			}
			items = append(items, page.Items...)
			if page.NextCursor == "" {
				break
			}
			query.Cursor = page.NextCursor
		}
		if err := s.notifications.ReconcileAttention(ctx, domain.ProjectID(project.ID), items); err != nil {
			return err
		}
	}
	return nil
}
