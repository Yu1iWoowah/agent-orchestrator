package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

const askPayload = `{"schemaVersion":1,"title":"Choose target","explanation":"Deployment needs your decision","questions":[{"key":"target","header":"Target","question":"Where?","options":[{"id":"staging","label":"Staging","description":"Test first","recommended":true},{"id":"production","label":"Production","description":"User-facing deployment"}]}]}`

func askCreate(session domain.SessionID) domain.AttentionQuestionCreate {
	return domain.AttentionQuestionCreate{ID: "question-1", OriginSessionID: session, ClientKey: "deploy", Audience: "user", Kind: domain.AttentionQuestionKindQuestion, Payload: []byte(askPayload)}
}

func askAnswer(id string) domain.AttentionAnswerSubmission {
	return domain.AttentionAnswerSubmission{QuestionID: id, ExpectedVersion: 1, ClientAnswerID: "answer-1", Answers: []domain.AttentionQuestionAnswer{{QuestionKey: "target", SelectedOptionIDs: []string{"staging"}}}}
}

func TestAttentionQuestionCreateIsValidatedScopedAndIdempotent(t *testing.T) {
	s, session, _ := conversationFixture(t)
	ctx := context.Background()
	command := askCreate(session)
	first, created, err := s.CreateAttentionQuestion(ctx, command, histClock)
	if err != nil || !created || first.Version != 1 || first.State != "open" || first.Read || first.ProjectID != "hist" {
		t.Fatalf("create = %+v, %v, %v", first, created, err)
	}
	command.ID = "ignored-retry-id"
	command.Payload = []byte(strings.Replace(askPayload, `"header":"Target"`, `"multiSelect":false,"header":"Target"`, 1))
	retry, created, err := s.CreateAttentionQuestion(ctx, command, histClock.Add(time.Second))
	if err != nil || created || retry.ID != first.ID {
		t.Fatalf("normalized retry = %+v, %v, %v", retry, created, err)
	}
	command.Payload = []byte(strings.Replace(askPayload, "Where?", "Changed?", 1))
	if _, _, err = s.CreateAttentionQuestion(ctx, command, histClock); !errors.Is(err, domain.ErrAttentionQuestionConflict) {
		t.Fatalf("key payload conflict = %v", err)
	}
	if _, err = s.GetAttentionQuestion(ctx, "different-session", first.ID); !errors.Is(err, domain.ErrAttentionQuestionNotFound) {
		t.Fatalf("foreign session read = %v", err)
	}
	command.ClientKey = "invalid"
	command.Payload = []byte(`{"schemaVersion":1}`)
	if _, _, err = s.CreateAttentionQuestion(ctx, command, histClock); err == nil {
		t.Fatal("invalid question committed")
	}
	page, err := s.ListAttentionQuestions(ctx, session, domain.AttentionQuestionQuery{}, histClock)
	if err != nil || len(page.Items) != 1 || page.Open != 1 || page.Unread != 1 {
		t.Fatalf("page after invalid = %+v, %v", page, err)
	}
}

func TestAttentionQuestionAnswerAtomicallyPersistsReceiptAndOneOutbox(t *testing.T) {
	s, session, _ := conversationFixture(t)
	ctx := context.Background()
	question, _, err := s.CreateAttentionQuestion(ctx, askCreate(session), histClock)
	if err != nil {
		t.Fatal(err)
	}
	answer := askAnswer(question.ID)
	bad := answer
	bad.Answers = []domain.AttentionQuestionAnswer{{QuestionKey: "target", SelectedOptionIDs: []string{"missing"}}}
	if _, err = s.AnswerAttentionQuestion(ctx, session, bad, histClock); err == nil {
		t.Fatal("invalid answer committed")
	}
	before, err := s.GetAttentionQuestion(ctx, session, question.ID)
	if err != nil || before.State != "open" || before.Answer != nil {
		t.Fatalf("invalid partial answer = %+v, %v", before, err)
	}
	for range 2 {
		receipt, err := s.AnswerAttentionQuestion(ctx, session, answer, histClock)
		if err != nil || receipt.ClientAnswerID != answer.ClientAnswerID || receipt.DeliveryState != "queued" {
			t.Fatalf("answer receipt = %+v, %v", receipt, err)
		}
	}
	answer.Answers[0].SelectedOptionIDs = []string{"production"}
	if _, err = s.AnswerAttentionQuestion(ctx, session, answer, histClock); !errors.Is(err, domain.ErrAttentionQuestionConflict) {
		t.Fatalf("changed retry = %v", err)
	}
	outbox, err := s.PendingAttentionAnswers(ctx, 10)
	if err != nil || len(outbox) != 1 || !strings.Contains(outbox[0].Message, "Test first") || !strings.Contains(outbox[0].Message, "question-1") || !strings.Contains(outbox[0].Message, "staging") {
		t.Fatalf("durable context = %+v, %v", outbox, err)
	}
	if err = s.RecordAttentionAnswerDelivery(ctx, question.ID, "delivered", "", histClock.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AnswerAttentionQuestion(ctx, session, askAnswer(question.ID), histClock); err != nil {
		t.Fatal(err)
	}
	outbox, err = s.PendingAttentionAnswers(ctx, 10)
	if err != nil || len(outbox) != 0 {
		t.Fatalf("retry redelivered = %+v, %v", outbox, err)
	}
	got, err := s.GetAttentionQuestion(ctx, session, question.ID)
	if err != nil || got.Answer == nil || got.Answer.DeliveryState != "delivered" {
		t.Fatalf("saved answer = %+v, %v", got, err)
	}
}

func TestAttentionQuestionVersionAndTerminalRaces(t *testing.T) {
	s, session, _ := conversationFixture(t)
	ctx := context.Background()
	q, _, err := s.CreateAttentionQuestion(ctx, askCreate(session), histClock)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkAttentionQuestionRead(ctx, session, q.ID, 1, histClock); err != nil {
		t.Fatal(err)
	}
	changed := []byte(strings.Replace(askPayload, "Where?", "New scope?", 1))
	q, err = s.ReviseAttentionQuestion(ctx, session, q.ID, 1, changed, histClock)
	if err != nil || q.Version != 2 || q.Read {
		t.Fatalf("revise = %+v, %v", q, err)
	}
	if err = s.MarkAttentionQuestionRead(ctx, session, q.ID, 1, histClock); !errors.Is(err, domain.ErrAttentionQuestionConflict) {
		t.Fatalf("old read = %v", err)
	}
	if _, err = s.AnswerAttentionQuestion(ctx, session, askAnswer(q.ID), histClock); !errors.Is(err, domain.ErrAttentionQuestionConflict) {
		t.Fatalf("old answer = %v", err)
	}
	answer := askAnswer(q.ID)
	answer.ExpectedVersion = 2
	var wait sync.WaitGroup
	results := make(chan error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, err := s.AnswerAttentionQuestion(ctx, session, answer, histClock)
		results <- err
	}()
	go func() {
		defer wait.Done()
		_, err := s.CancelAttentionQuestion(ctx, session, q.ID, 2, "user", "No longer needed", histClock)
		results <- err
	}()
	wait.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, domain.ErrAttentionQuestionConflict) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("terminal winners = %d", wins)
	}
	if _, err = s.ReviseAttentionQuestion(ctx, session, q.ID, 2, changed, histClock); !errors.Is(err, domain.ErrAttentionQuestionConflict) {
		t.Fatalf("terminal reopened = %v", err)
	}
}

func TestAttentionQuestionDeadlineExpiresButIdleDoesNot(t *testing.T) {
	s, session, _ := conversationFixture(t)
	ctx := context.Background()
	cmd := askCreate(session)
	deadline := histClock.Add(time.Minute)
	cmd.ExpiresAt = &deadline
	q, _, err := s.CreateAttentionQuestion(ctx, cmd, histClock)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListAttentionQuestions(ctx, session, domain.AttentionQuestionQuery{}, histClock.Add(30*time.Second))
	if err != nil || page.Open != 1 {
		t.Fatalf("idle expired question: %+v %v", page, err)
	}
	if _, err = s.AnswerAttentionQuestion(ctx, session, askAnswer(q.ID), deadline); !errors.Is(err, domain.ErrAttentionQuestionConflict) {
		t.Fatalf("deadline answer = %v", err)
	}
	page, err = s.ListAttentionQuestions(ctx, session, domain.AttentionQuestionQuery{State: "history"}, deadline)
	if err != nil || page.Open != 0 || page.History != 1 || len(page.Items) != 1 || page.Items[0].State != "expired" {
		t.Fatalf("expired page = %+v, %v", page, err)
	}
}

func TestAttentionQuestionCreateRetryAfterDeadlineReturnsOriginal(t *testing.T) {
	s, session, _ := conversationFixture(t)
	ctx := context.Background()
	cmd := askCreate(session)
	deadline := histClock.Add(time.Minute)
	cmd.ExpiresAt = &deadline
	original, _, err := s.CreateAttentionQuestion(ctx, cmd, histClock)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ExpireAttentionQuestions(ctx, session, deadline); err != nil {
		t.Fatal(err)
	}
	cmd.ID = "different-id"
	got, created, err := s.CreateAttentionQuestion(ctx, cmd, deadline.Add(time.Minute))
	if err != nil || created || got.ID != original.ID || got.State != "expired" {
		t.Fatalf("expired create retry = %+v %v %v", got, created, err)
	}
}

func TestAttentionQuestionGroupedAnswerHasNoPartialCommit(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	cmd := askCreate(session)
	cmd.Payload = []byte(strings.Replace(askPayload, `]}]}`, `]}, {"key":"region","header":"Region","question":"Which region?","options":[{"id":"tw","label":"Taiwan","description":"Local region"},{"id":"jp","label":"Japan","description":"Nearby region"}]}]}`, 1))
	q, _, err := s.CreateAttentionQuestion(ctx, cmd, histClock)
	if err != nil {
		t.Fatal(err)
	}
	answer := askAnswer(q.ID)
	if _, err = s.AnswerAttentionQuestion(ctx, session, answer, histClock); err == nil {
		t.Fatal("partial grouped answer accepted")
	}
	got, err := s.GetAttentionQuestion(ctx, session, q.ID)
	if err != nil || got.State != "open" || got.Answer != nil {
		t.Fatalf("partial = %+v %v", got, err)
	}
	answer.Answers = append(answer.Answers, domain.AttentionQuestionAnswer{QuestionKey: "region", SelectedOptionIDs: []string{"tw"}})
	if _, err = s.AnswerAttentionQuestion(ctx, session, answer, histClock); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.LoadConversationSnapshot(ctx, conversation)
	if err != nil || len(snapshot.Activities) != 2 {
		t.Fatalf("compatibility records = %+v %v", snapshot.Activities, err)
	}
	for _, activity := range snapshot.Activities {
		if activity.Kind != domain.ActivityKindSystem || !strings.Contains(activity.Summary, "Which region?") || !strings.Contains(activity.Summary, "Production") || !strings.Contains(string(activity.Detail), "question-1") {
			t.Fatalf("unreadable compatibility record: %+v", activity)
		}
	}
	providerPage, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{})
	if err != nil || providerPage.Counts.Open != 0 {
		t.Fatalf("question duplicated into native provider index = %+v %v", providerPage, err)
	}
}

func TestAttentionQuestionSavedAnswerSurvivesRestartAndSessionTerminationExpiresOnlyOpen(t *testing.T) {
	directory := t.TempDir()
	s, err := sqlite.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedProject(t, s, "hist")
	owner, err := s.CreateSession(ctx, sampleRecord("hist"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := s.CreateAttentionQuestion(ctx, askCreate(owner.ID), histClock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AnswerAttentionQuestion(ctx, owner.ID, askAnswer(first.ID), histClock); err != nil {
		t.Fatal(err)
	}
	second := askCreate(owner.ID)
	second.ID = "question-2"
	second.ClientKey = "followup"
	if _, _, err = s.CreateAttentionQuestion(ctx, second, histClock); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = sqlite.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingAttentionAnswers(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("restart lost answer: %+v %v", pending, err)
	}
	owner.IsTerminated = true
	owner.UpdatedAt = histClock.Add(time.Minute)
	if err = s.UpdateSession(ctx, owner); err != nil {
		t.Fatal(err)
	}
	closed, err := s.GetAttentionQuestion(ctx, owner.ID, second.ID)
	if err != nil || closed.State != "expired" || closed.CloseReason != "origin_session_terminated" {
		t.Fatalf("termination = %+v %v", closed, err)
	}
	answered, err := s.GetAttentionQuestion(ctx, owner.ID, first.ID)
	if err != nil || answered.State != "answered" || answered.Answer == nil || answered.Answer.DeliveryState != "queued" {
		t.Fatalf("termination lost saved answer = %+v %v", answered, err)
	}
}

func TestAttentionQuestionAgentReplacementExpiresOriginalSource(t *testing.T) {
	s, session, _ := conversationFixture(t)
	ctx := context.Background()
	q, _, err := s.CreateAttentionQuestion(ctx, askCreate(session), histClock)
	if err != nil {
		t.Fatal(err)
	}
	rec, found, err := s.GetSession(ctx, session)
	if err != nil || !found {
		t.Fatalf("session %v %v", found, err)
	}
	rec.Harness = domain.HarnessCodex
	rec.UpdatedAt = histClock.Add(time.Second)
	if err = s.UpdateSession(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAttentionQuestion(ctx, session, q.ID)
	if err != nil || got.State != "expired" || got.CloseReason != "origin_agent_replaced" {
		t.Fatalf("replacement source = %+v %v", got, err)
	}
}

func TestAttentionQuestionStaleRemovedOptionConflictsBeforeValidation(t *testing.T) {
	s, session, _ := conversationFixture(t)
	ctx := context.Background()
	q, _, err := s.CreateAttentionQuestion(ctx, askCreate(session), histClock)
	if err != nil {
		t.Fatal(err)
	}
	revised := []byte(strings.Replace(askPayload, `"id":"staging"`, `"id":"preview"`, 1))
	if _, err = s.ReviseAttentionQuestion(ctx, session, q.ID, 1, revised, histClock); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AnswerAttentionQuestion(ctx, session, askAnswer(q.ID), histClock); !errors.Is(err, domain.ErrAttentionQuestionConflict) {
		t.Fatalf("stale removed option must conflict, got %v", err)
	}
	got, err := s.GetAttentionQuestion(ctx, session, q.ID)
	if err != nil || got.State != "open" || got.Answer != nil {
		t.Fatalf("stale write changed state: %+v %v", got, err)
	}
}

func TestAttentionAnswerBusyBatchDoesNotStarveLaterAnswers(t *testing.T) {
	s, session, _ := conversationFixture(t)
	ctx := context.Background()
	for i := 0; i < 21; i++ {
		create := askCreate(session)
		create.ID = fmt.Sprintf("question-%02d", i)
		create.ClientKey = create.ID
		q, _, err := s.CreateAttentionQuestion(ctx, create, histClock)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.AnswerAttentionQuestion(ctx, session, askAnswer(q.ID), histClock); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		first, err := s.PendingAttentionAnswers(ctx, 20)
		if err != nil || len(first) != 20 {
			t.Fatalf("batch: %+v %v", first, err)
		}
		for _, answer := range first {
			if err = s.RecordAttentionAnswerDelivery(ctx, answer.QuestionID, "queued", "busy", histClock); err != nil {
				t.Fatal(err)
			}
		}
		next, err := s.PendingAttentionAnswers(ctx, 20)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, answer := range next {
			if answer.QuestionID == "question-20" {
				found = true
			}
		}
		if !found {
			t.Fatal("busy batch starves later saved answer")
		}
	}
}

func TestAttentionAnswerNativeAcceptanceSurvivesLostOutboxAcknowledgment(t *testing.T) {
	directory := t.TempDir()
	s, err := sqlite.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedProject(t, s, "hist")
	rec := sampleRecord("hist")
	rec.Mode = domain.SessionModeTUI
	owner, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	owner.Metadata.RuntimeLaunchID = "launch-1"
	if err = s.UpdateSession(ctx, owner); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"question-a", "question-b"} {
		create := askCreate(owner.ID)
		create.ID = id
		create.ClientKey = id
		if _, _, err = s.CreateAttentionQuestion(ctx, create, histClock); err != nil {
			t.Fatal(err)
		}
		if _, err = s.AnswerAttentionQuestion(ctx, owner.ID, askAnswer(id), histClock); err != nil {
			t.Fatal(err)
		}
	}
	owner.Metadata.ConversationCheckpointState = domain.ConversationCheckpointCoordination
	owner.Metadata.ConversationCheckpointGeneration = "wrong-generation"
	owner.Metadata.ConversationCheckpointTurnID = "question-answer-question-a"
	owner.UpdatedAt = histClock.Add(time.Second)
	if err = s.UpdateSession(ctx, owner); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetAttentionQuestion(ctx, owner.ID, "question-a")
	if err != nil || before.Answer.DeliveryState != "queued" {
		t.Fatalf("foreign generation ack: %+v %v", before, err)
	}
	owner.Metadata.ConversationCheckpointGeneration = "launch-1"
	if err = s.UpdateSession(ctx, owner); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAttentionQuestion(ctx, owner.ID, "question-a")
	if err != nil || got.Answer.DeliveryState != "delivered" {
		t.Fatalf("native acceptance must atomically acknowledge saved answer: %+v %v", got, err)
	}
	saved, found, err := s.GetSession(ctx, owner.ID)
	if err != nil || !found || !strings.Contains(saved.Metadata.LatestUserPrompt, "question-a") || saved.Metadata.ConversationCheckpointTurnID != "question-answer-question-a" {
		t.Fatalf("human provenance/checkpoint lost: %+v %v", saved, err)
	}
	saved.Metadata.ConversationCheckpointTurnID = "later-prompt"
	saved.Metadata.LatestUserPrompt = "later ordinary message"
	later := histClock.Add(time.Minute)
	saved.Metadata.LatestUserPromptAt = later
	if err = s.UpdateSession(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = sqlite.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingAttentionAnswers(ctx, 20)
	if err != nil || len(pending) != 1 || pending[0].QuestionID != "question-b" {
		t.Fatalf("restart would redeliver accepted answer: %+v %v", pending, err)
	}
}

func TestAttentionQuestionConcurrentTransitions(t *testing.T) {
	for _, transition := range []string{"answer", "revise", "expire"} {
		t.Run(transition, func(t *testing.T) {
			s, session, _ := conversationFixture(t)
			ctx := context.Background()
			create := askCreate(session)
			deadline := histClock.Add(time.Minute)
			if transition == "expire" {
				create.ExpiresAt = &deadline
			}
			q, _, err := s.CreateAttentionQuestion(ctx, create, histClock)
			if err != nil {
				t.Fatal(err)
			}
			at := histClock
			if transition == "expire" {
				at = deadline
			}
			results := make(chan error, 2)
			start := make(chan struct{})
			var wait sync.WaitGroup
			wait.Add(2)
			go func() {
				defer wait.Done()
				<-start
				_, err := s.AnswerAttentionQuestion(ctx, session, askAnswer(q.ID), at)
				results <- err
			}()
			go func() {
				defer wait.Done()
				<-start
				var err error
				switch transition {
				case "answer":
					other := askAnswer(q.ID)
					other.ClientAnswerID = "other-client"
					_, err = s.AnswerAttentionQuestion(ctx, session, other, at)
				case "revise":
					_, err = s.ReviseAttentionQuestion(ctx, session, q.ID, 1, []byte(strings.Replace(askPayload, `"id":"staging"`, `"id":"preview"`, 1)), at)
				case "expire":
					err = s.ExpireAttentionQuestions(ctx, session, at)
				}
				results <- err
			}()
			close(start)
			wait.Wait()
			close(results)
			success, conflicts := 0, 0
			for err := range results {
				if err == nil {
					success++
				} else if errors.Is(err, domain.ErrAttentionQuestionConflict) {
					conflicts++
				} else {
					t.Fatal(err)
				}
			}
			if success != 1 || conflicts != 1 {
				t.Fatalf("success=%d conflicts=%d", success, conflicts)
			}
			got, err := s.GetAttentionQuestion(ctx, session, q.ID)
			if err != nil {
				t.Fatal(err)
			}
			queued, err := s.PendingAttentionAnswers(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			switch got.State {
			case "answered":
				if transition == "expire" || got.Answer == nil || len(queued) != 1 || got.Version != 1 {
					t.Fatalf("invalid winning answer: %+v %+v", got, queued)
				}
			case "open":
				if transition != "revise" || got.Version != 2 || got.Answer != nil || len(queued) != 0 {
					t.Fatalf("invalid revision: %+v %+v", got, queued)
				}
			case "expired":
				if transition != "expire" || got.Answer != nil || len(queued) != 0 {
					t.Fatalf("expired answer committed: %+v %+v", got, queued)
				}
			default:
				t.Fatalf("unexpected state %q", got.State)
			}
		})
	}
}

func TestAttentionQuestionCancelReasonUnicodeBoundary(t *testing.T) {
	s, session, _ := conversationFixture(t)
	ctx := context.Background()
	q, _, err := s.CreateAttentionQuestion(ctx, askCreate(session), histClock)
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"", " \t", strings.Repeat("🚀", 4001)} {
		if _, err = s.CancelAttentionQuestion(ctx, session, q.ID, 1, "agent", reason, histClock); err == nil {
			t.Fatal("invalid reason accepted")
		}
	}
	reason := strings.Repeat("🚀", 4000)
	got, err := s.CancelAttentionQuestion(ctx, session, q.ID, 1, "agent", reason, histClock)
	if err != nil || got.State != "cancelled" || got.CloseReason != reason {
		t.Fatalf("Unicode reason boundary: state=%s error=%v", got.State, err)
	}
}
