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

type Store interface {
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

type Delivery interface {
	Submit(context.Context, domain.SessionID, string, string) error
}
type Deps struct {
	Store    Store
	Delivery Delivery
	Now      func() time.Time
	NewID    func() string
}
type Service struct {
	store      Store
	delivery   Delivery
	now        func() time.Time
	newID      func() string
	wake       chan struct{}
	deliveryMu sync.Mutex
}

func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.NewID == nil {
		deps.NewID = func() string { return "ask_" + uuid.NewString() }
	}
	return &Service{store: deps.Store, delivery: deps.Delivery, now: deps.Now, newID: deps.NewID, wake: make(chan struct{}, 1)}
}

func (s *Service) Create(ctx context.Context, input domain.AttentionQuestionCreate) (domain.AttentionQuestionRecord, bool, error) {
	input.ID = s.newID()
	return s.store.CreateAttentionQuestion(ctx, input, s.now().UTC())
}

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

func (s *Service) Get(ctx context.Context, session domain.SessionID, id string) (domain.AttentionQuestionRecord, error) {
	if err := s.store.ExpireAttentionQuestions(ctx, session, s.now().UTC()); err != nil {
		return domain.AttentionQuestionRecord{}, err
	}
	return s.store.GetAttentionQuestion(ctx, session, id)
}

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

func (s *Service) Revise(ctx context.Context, session domain.SessionID, id string, version int64, payload []byte) (domain.AttentionQuestionRecord, error) {
	return s.store.ReviseAttentionQuestion(ctx, session, id, version, payload, s.now().UTC())
}

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
		if err = s.store.ExpireAttentionQuestions(ctx, session, s.now().UTC()); err != nil {
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
	for _, item := range items {
		if err = ctx.Err(); err != nil {
			return err
		}
		owner, found, err := s.store.GetSession(ctx, item.OriginSessionID)
		if err != nil {
			return err
		}
		if !found || owner.IsTerminated || string(owner.Harness) != item.OriginHarness {
			if err = s.store.RecordAttentionAnswerDelivery(ctx, item.QuestionID, "failed", "Origin session or agent is no longer available; the answer remains saved", s.now().UTC()); err != nil {
				return err
			}
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, 15*time.Second)
		sendErr := s.delivery.Submit(attempt, item.OriginSessionID, item.Message, item.ClientMessageID)
		cancel()
		if err = ctx.Err(); err != nil {
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
		if err = s.store.RecordAttentionAnswerDelivery(ctx, item.QuestionID, state, lastError, s.now().UTC()); err != nil {
			return err
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
