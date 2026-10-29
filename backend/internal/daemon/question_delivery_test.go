package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type questionDeliverySession struct {
	id           domain.SessionID
	message, key string
	options      ports.MessageDeliveryOptions
	err          error
}

func (s *questionDeliverySession) SendSemanticWithOptions(_ context.Context, id domain.SessionID, message, key string, options ports.MessageDeliveryOptions) error {
	s.id, s.message, s.key, s.options = id, message, key, options
	return s.err
}

func TestQuestionAnswerDeliveryUsesHumanSemanticBoundary(t *testing.T) {
	now := time.Date(2026, 10, 29, 5, 0, 0, 0, time.UTC)
	sessions := &questionDeliverySession{}
	delivery := questionSemanticDelivery{sessions: sessions, now: func() time.Time { return now }}
	if err := delivery.Submit(context.Background(), "worker-1", "User chose Staging", "question-answer-ask-1"); err != nil {
		t.Fatal(err)
	}
	if sessions.id != "worker-1" || sessions.key != "question-answer-ask-1" || sessions.message != "User chose Staging" || !sessions.options.AuthoredByUser || !sessions.options.InteractionAt.Equal(now) {
		t.Fatalf("human semantic boundary: %+v", sessions)
	}
	sessions.err = errors.New("native composer busy")
	if err := delivery.Submit(context.Background(), "worker-1", "answer", sessions.key); !errors.Is(err, sessions.err) {
		t.Fatalf("lost refusal: %v", err)
	}
}
