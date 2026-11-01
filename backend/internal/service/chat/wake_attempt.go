package chat

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type chatWakeJournal interface {
	SaveChatWakeAttempt(context.Context, domain.ChatWakeAttempt, int64) (domain.ChatWakeAttempt, error)
}

func (s *Service) beginWakeAttempt(ctx context.Context, id domain.SessionID) (*domain.ChatWakeAttempt, error) {
	journal, ok := s.store.(chatWakeJournal)
	if !ok {
		return nil, nil
	}
	at := s.now()
	a, err := journal.SaveChatWakeAttempt(ctx, domain.ChatWakeAttempt{ID: uuid.NewString(), SessionID: id, Phase: "waking", CreatedAt: at, UpdatedAt: at}, -1)
	return &a, err
}
func (s *Service) saveWakeAttempt(ctx context.Context, a *domain.ChatWakeAttempt, phase, code string) error {
	if a == nil {
		return nil
	}
	next := *a
	next.Phase, next.ErrorCode, next.UpdatedAt, next.Version = phase, code, s.now(), a.Version+1
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	journal, ok := s.store.(chatWakeJournal)
	if !ok {
		return nil
	}
	saved, err := journal.SaveChatWakeAttempt(saveCtx, next, a.Version)
	if err == nil {
		*a = saved
	}
	return err
}
