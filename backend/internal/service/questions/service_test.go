package questions_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const payload = `{"schemaVersion":1,"title":"Choose a target","explanation":"Deployment requires your decision","questions":[{"key":"target","header":"Target","question":"Where?","options":[{"id":"stage","label":"Staging","description":"Internal users"},{"id":"prod","label":"Production","description":"Public users"}]}]}`

var clock = time.Date(2026, 10, 29, 4, 0, 0, 0, time.UTC)

type delivery struct {
	err      error
	messages []string
	keys     []string
	accepted map[string]bool
}

func (d *delivery) Submit(_ context.Context, _ domain.SessionID, message, key string) error {
	d.keys = append(d.keys, key)
	d.messages = append(d.messages, message)
	if d.accepted == nil {
		d.accepted = map[string]bool{}
	}
	d.accepted[key] = true
	return d.err
}

func fixture(t *testing.T) (*sqlite.Store, domain.SessionID) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "questions", Path: t.TempDir(), RegisteredAt: clock}); err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "questions", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: clock}, CreatedAt: clock, UpdatedAt: clock})
	if err != nil {
		t.Fatal(err)
	}
	return store, session.ID
}

func TestQuestionsSaveBeforeDeliveryAndRecoverSameMessageAfterRestart(t *testing.T) {
	store, session := fixture(t)
	ctx := context.Background()
	target := &delivery{err: errors.New("provider busy after acceptance")}
	newService := func() *questions.Service {
		return questions.New(questions.Deps{Store: store, Delivery: target, Now: func() time.Time { return clock }, NewID: func() string { return "question-1" }})
	}
	svc := newService()
	q, _, err := svc.Create(ctx, domain.AttentionQuestionCreate{OriginSessionID: session, ClientKey: "deployment", Audience: "user", Kind: domain.AttentionQuestionKindQuestion, Payload: []byte(payload)})
	if err != nil {
		t.Fatal(err)
	}
	answer := domain.AttentionAnswerSubmission{QuestionID: q.ID, ExpectedVersion: 1, ClientAnswerID: "answer-1", Answers: []domain.AttentionQuestionAnswer{{QuestionKey: "target", SelectedOptionIDs: []string{"stage"}}}}
	receipt, err := svc.Answer(ctx, session, answer)
	if err != nil || receipt.DeliveryState != "queued" || len(target.keys) != 0 {
		t.Fatalf("save did not precede delivery: %+v %v calls=%d", receipt, err, len(target.keys))
	}
	if err = svc.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, session, q.ID)
	if err != nil || got.State != "answered" || got.Answer == nil || got.Answer.DeliveryState != "queued" || got.Answer.LastError == "" {
		t.Fatalf("failure lost receipt: %+v %v", got, err)
	}
	// A new coordinator retries the same stable message after a lost acknowledgement.
	target.err = nil
	svc = newService()
	if err = svc.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if len(target.keys) != 2 || target.keys[0] != target.keys[1] || len(target.accepted) != 1 {
		t.Fatalf("retry changed semantic id: %+v", target.keys)
	}
	if !strings.Contains(target.messages[0], "Internal users") || !strings.Contains(target.messages[0], "human direction") {
		t.Fatalf("missing answer context: %q", target.messages[0])
	}
	got, err = svc.Get(ctx, session, q.ID)
	if err != nil || got.Answer.DeliveryState != "delivered" {
		t.Fatalf("accepted delivery: %+v %v", got, err)
	}
	owner, found, err := store.GetSession(ctx, session)
	if err != nil || !found || !strings.Contains(owner.Metadata.LatestUserPrompt, "Internal users") {
		t.Fatalf("accepted human direction = %+v %v", owner.Metadata, err)
	}
	if _, err = svc.Answer(ctx, session, answer); err != nil {
		t.Fatal(err)
	}
	if err = svc.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if len(target.keys) != 2 {
		t.Fatalf("idempotent answer sent again: %+v", target.keys)
	}
}

func TestQuestionsUnavailableOriginKeepsReadableUndeliveredAnswer(t *testing.T) {
	for _, change := range []string{"terminated", "agent replaced"} {
		t.Run(change, func(t *testing.T) {
			store, session := fixture(t)
			ctx := context.Background()
			target := &delivery{}
			svc := questions.New(questions.Deps{Store: store, Delivery: target, Now: func() time.Time { return clock }})
			q, _, err := svc.Create(ctx, domain.AttentionQuestionCreate{OriginSessionID: session, ClientKey: "deploy", Audience: "user", Kind: domain.AttentionQuestionKindQuestion, Payload: []byte(payload)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.Answer(ctx, session, domain.AttentionAnswerSubmission{QuestionID: q.ID, ExpectedVersion: 1, ClientAnswerID: "answer", Answers: []domain.AttentionQuestionAnswer{{QuestionKey: "target", SelectedOptionIDs: []string{"stage"}}}}); err != nil {
				t.Fatal(err)
			}
			rec, ok, err := store.GetSession(ctx, session)
			if err != nil || !ok {
				t.Fatalf("session %v %v", ok, err)
			}
			if change == "terminated" {
				rec.IsTerminated = true
			} else {
				rec.Harness = domain.HarnessCodex
			}
			if err = store.UpdateSession(ctx, rec); err != nil {
				t.Fatal(err)
			}
			if err = svc.DeliverPending(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := svc.Get(ctx, session, q.ID)
			if err != nil || got.Answer == nil || got.Answer.DeliveryState != "failed" || got.Answer.LastError == "" || len(target.keys) != 0 {
				t.Fatalf("terminated delivery = %+v %v calls=%d", got, err, len(target.keys))
			}
		})
	}
}

func TestQuestionsDeadlineSweepDoesNotRequireAClientRead(t *testing.T) {
	store, session := fixture(t)
	ctx := context.Background()
	now := clock
	index := 0
	svc := questions.New(questions.Deps{Store: store, Now: func() time.Time { return now }, NewID: func() string { index++; return fmt.Sprintf("deadline-%d", index) }})
	deadline := now.Add(time.Minute)
	command := domain.AttentionQuestionCreate{OriginSessionID: session, ClientKey: "deadline", Audience: "user", Kind: domain.AttentionQuestionKindQuestion, Payload: []byte(payload), ExpiresAt: &deadline}
	expiring, _, err := svc.Create(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	command.ClientKey = "no-deadline"
	command.ExpiresAt = nil
	permanent, _, err := svc.Create(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	now = deadline
	if err = svc.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	expired, err := store.GetAttentionQuestion(ctx, session, expiring.ID)
	if err != nil || expired.State != "expired" {
		t.Fatalf("background expiration = %+v %v", expired, err)
	}
	open, err := store.GetAttentionQuestion(ctx, session, permanent.ID)
	if err != nil || open.State != "open" {
		t.Fatalf("default question expired = %+v %v", open, err)
	}
}

type deliveryFunc func(context.Context, domain.SessionID, string, string) error

func (f deliveryFunc) Submit(ctx context.Context, id domain.SessionID, message, key string) error {
	return f(ctx, id, message, key)
}

func TestQuestionsCoordinatorShutdownCancelsDeliveryWithoutLosingSavedAnswer(t *testing.T) {
	store, session := fixture(t)
	started := make(chan struct{})
	svc := questions.New(questions.Deps{Store: store, Now: func() time.Time { return clock }, Delivery: deliveryFunc(func(ctx context.Context, _ domain.SessionID, _, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})})
	q, _, err := svc.Create(context.Background(), domain.AttentionQuestionCreate{OriginSessionID: session, ClientKey: "shutdown", Audience: "user", Kind: domain.AttentionQuestionKindQuestion, Payload: []byte(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Answer(context.Background(), session, domain.AttentionAnswerSubmission{QuestionID: q.ID, ExpectedVersion: 1, ClientAnswerID: "answer", Answers: []domain.AttentionQuestionAnswer{{QuestionKey: "target", SelectedOptionIDs: []string{"stage"}}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := svc.Start(ctx, func(err error) { t.Errorf("coordinator: %v", err) })
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not restore persisted work")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not stop")
	}
	got, err := store.GetAttentionQuestion(context.Background(), session, q.ID)
	if err != nil || got.State != "answered" || got.Answer == nil || got.Answer.DeliveryState != "queued" {
		t.Fatalf("shutdown lost saved answer: %+v %v", got, err)
	}
}
