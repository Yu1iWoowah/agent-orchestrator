package questions

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type acceptedAnswerStore struct {
	Store
	gets int
}

func (s *acceptedAnswerStore) AttentionQuestionExpirationSessions(context.Context, time.Time) ([]domain.SessionID, error) {
	return nil, nil
}
func (s *acceptedAnswerStore) PendingAttentionAnswers(context.Context, int64) ([]domain.AttentionAnswerDelivery, error) {
	return nil, nil
}
func (s *acceptedAnswerStore) GetAttentionQuestion(context.Context, domain.SessionID, string) (domain.AttentionQuestionRecord, error) {
	s.gets++
	return domain.AttentionQuestionRecord{Answer: &domain.AttentionAnswerReceipt{DeliveryState: "delivered"}}, nil
}

type noDelivery struct{}

func (noDelivery) Submit(context.Context, domain.SessionID, string, string) error { return nil }

func TestNativeAcceptedAnswersReleaseRetryMetadata(t *testing.T) {
	st := &acceptedAnswerStore{}
	svc := New(Deps{Store: st, Delivery: noDelivery{}})
	svc.retries["question"] = answerRetry{session: "worker", at: time.Now().Add(time.Hour)}
	if err := svc.DeliverPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(svc.retries) != 0 {
		t.Fatal("native accepted answer retained retry metadata")
	}
	if err := svc.DeliverPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.gets != 1 {
		t.Fatalf("terminal retry should be checked once, got %d", st.gets)
	}
}
