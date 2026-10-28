package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func attentionActivity(t *testing.T, s *sqlite.Store, conversation, turn, id string, kind domain.ActivityKind, status domain.ActivityStatus) {
	t.Helper()
	if err := s.UpsertActivity(context.Background(), conversation, "provider-"+turn, domain.ConversationActivity{
		ID: id, ProviderItemID: id, RequestID: "request-" + id,
		Kind: kind, Status: status, Summary: "Review " + id,
		Detail: []byte(`{"message":"Choose a deployment target","decisions":[{"id":"allow","label":"Allow","kind":"allow_once"}]}`),
	}, histClock); err != nil {
		t.Fatal(err)
	}
}

func TestConversationAttentionCountsDoNotDependOnTranscriptOrPage(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "attention", histClock)
	attentionActivity(t, s, conversation, "attention", "one", domain.ActivityKindApproval, domain.ActivityStatusPending)
	attentionActivity(t, s, conversation, "attention", "two", domain.ActivityKindUserInput, domain.ActivityStatusPending)
	attentionActivity(t, s, conversation, "attention", "three", domain.ActivityKindApproval, domain.ActivityStatusPending)
	attentionActivity(t, s, conversation, "attention", "resolved", domain.ActivityKindUserInput, domain.ActivityStatusResolved)
	attentionActivity(t, s, conversation, "attention", "ordinary-tool", domain.ActivityKindCommand, domain.ActivityStatusRunning)
	for i := range 205 {
		appendPlanTurn(t, s, conversation, session, fmt.Sprintf("later-%d", i), histClock)
	}
	page, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "open", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Activity.ID != "three" || page.Counts.Open != 3 || page.Counts.Unread != 3 || page.Counts.History != 1 || !page.HasMoreBefore {
		t.Fatalf("first attention page = %+v, %v", page, err)
	}
	older, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "open", Limit: 2, BeforeSequence: page.NextBeforeSequence})
	if err != nil || len(older.Items) != 2 || older.Items[0].Activity.ID != "two" || older.Items[1].Activity.ID != "one" || older.HasMoreBefore || older.Counts != page.Counts {
		t.Fatalf("older attention page = %+v, %v", older, err)
	}
	history, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "history", Limit: 20})
	if err != nil || len(history.Items) != 1 || history.Items[0].Activity.ID != "resolved" || history.Items[0].Actionable {
		t.Fatalf("history = %+v, %v", history, err)
	}
}

func TestConversationAttentionReadIsVersionedAndNeverResolvesRequest(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "attention", histClock)
	attentionActivity(t, s, conversation, "attention", "one", domain.ActivityKindApproval, domain.ActivityStatusPending)
	page, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "open", Limit: 20})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("initial = %+v, %v", page, err)
	}
	revision := page.Items[0].Activity.Revision
	for range 2 {
		if err := s.MarkConversationAttentionRead(ctx, session, conversation, page.ActiveBranchID, "one", revision, histClock); err != nil {
			t.Fatal(err)
		}
	}
	seen, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "open", Limit: 20})
	if err != nil || seen.Counts.Open != 1 || seen.Counts.Unread != 0 || !seen.Items[0].Read || seen.Items[0].Activity.Status != domain.ActivityStatusPending {
		t.Fatalf("read changed pending truth = %+v, %v", seen, err)
	}
	if err := s.UpsertActivity(ctx, conversation, "provider-attention", domain.ConversationActivity{ID: "one", ProviderItemID: "one", RequestID: "request-one", Kind: domain.ActivityKindApproval, Status: domain.ActivityStatusPending, Summary: "Changed scope", Detail: []byte(`{"command":"changed scope"}`)}, histClock.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConversationAttentionRead(ctx, session, conversation, page.ActiveBranchID, "one", revision, histClock); !errors.Is(err, domain.ErrAttentionChanged) {
		t.Fatalf("stale revision marked current request read: %v", err)
	}
	changed, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "open", Limit: 20})
	if err != nil || changed.Counts.Open != 1 || changed.Counts.Unread != 1 || changed.Items[0].Read {
		t.Fatalf("changed content = %+v, %v", changed, err)
	}
	if err := s.ResolveApproval(ctx, conversation, "request-one", `{"decision":"allow"}`, histClock); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "open", Limit: 20})
	if err != nil || resolved.Counts.Open != 0 || resolved.Counts.History != 1 || len(resolved.Items) != 0 {
		t.Fatalf("resolved source stayed open = %+v, %v", resolved, err)
	}
}

func TestAttentionReplayDoesNotMakeReadRequestUnread(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "attention", histClock)
	attentionActivity(t, s, conversation, "attention", "one", domain.ActivityKindApproval, domain.ActivityStatusPending)
	page, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConversationAttentionRead(ctx, session, conversation, page.ActiveBranchID, "one", page.Items[0].Activity.Revision, histClock); err != nil {
		t.Fatal(err)
	}
	attentionActivity(t, s, conversation, "attention", "one", domain.ActivityKindApproval, domain.ActivityStatusPending)
	replayed, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "open"})
	if err != nil || replayed.Counts.Open != 1 || replayed.Counts.Unread != 0 || !replayed.Items[0].Read {
		t.Fatalf("identical replay became new unread content: %+v, %v", replayed, err)
	}
}

func TestConversationAttentionDoesNotExposeOtherBranchesOrSessions(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "attention", histClock)
	attentionActivity(t, s, conversation, "attention", "inherited", domain.ActivityKindApproval, domain.ActivityStatusPending)
	root, err := s.ConversationForSession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "open", Limit: 20})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("root = %+v, %v", page, err)
	}
	cutoff := page.Items[0].Activity.Sequence
	attentionActivity(t, s, conversation, "attention", "parent-future", domain.ActivityKindUserInput, domain.ActivityStatusPending)
	child := domain.ConversationBranch{ID: "attention-child", ConversationID: conversation, SessionID: session, ProviderConversationID: "attention-child-thread", ParentBranchID: root.ActiveBranchID, ForkAfterSequence: cutoff}
	if err := s.CreateConversationBranch(ctx, child, histClock); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConversationBranch(ctx, session, conversation, child.ID, child.ProviderConversationID, "child-generation", histClock); err != nil {
		t.Fatal(err)
	}
	visible, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "history", Limit: 20})
	if err != nil || visible.ActiveBranchID != child.ID || visible.Counts.Open != 0 || len(visible.Items) != 1 || visible.Items[0].Activity.ID != "inherited" || visible.Items[0].Actionable {
		t.Fatalf("branch visibility/actionability = %+v, %v", visible, err)
	}
	if err := s.MarkConversationAttentionRead(ctx, session, conversation, root.ActiveBranchID, "inherited", page.Items[0].Activity.Revision, histClock); !errors.Is(err, domain.ErrAttentionChanged) {
		t.Fatalf("old branch accepted: %v", err)
	}
	if _, err := s.ConversationAttention(ctx, "another-session", conversation, domain.ConversationAttentionQuery{State: "open", Limit: 20}); err == nil {
		t.Fatal("another session could read this conversation's attention")
	}
}

func TestConversationAttentionReadDoesNotWaitForProviderWriter(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	appendPlanTurn(t, s, conversation, session, "attention", histClock)
	attentionActivity(t, s, conversation, "attention", "one", domain.ActivityKindApproval, domain.ActivityStatusPending)
	entered, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		_, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", "attention-held", "test.pending", `{}`, histClock, func(context.Context) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		writer <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		close(release)
		<-writer
		t.Fatal("provider did not enter writer transaction")
	}
	read := make(chan error, 1)
	go func() {
		page, err := s.ConversationAttention(ctx, session, conversation, domain.ConversationAttentionQuery{State: "open"})
		if err == nil && page.Counts.Open != 1 {
			err = fmt.Errorf("open count=%d", page.Counts.Open)
		}
		read <- err
	}()
	var readErr error
	timedOut := false
	select {
	case readErr = <-read:
	case <-time.After(2 * time.Second):
		timedOut = true
	}
	close(release)
	writerErr := <-writer
	if timedOut {
		readErr = <-read
	}
	if timedOut || readErr != nil || writerErr != nil {
		t.Fatalf("read waited=%v read=%v writer=%v", timedOut, readErr, writerErr)
	}
}

func TestConversationAttentionReadPersistsAndEmitsOnlyOneCDCEvent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedProject(t, s, "attention")
	rec := sampleRecord("attention")
	rec.Mode = domain.SessionModeChat
	session, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := s.CreateConversation(ctx, "attention-reopen", domain.ConversationScopeSession, "attention", session.ID, histClock)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-1"); err != nil {
		t.Fatal(err)
	}
	appendPlanTurn(t, s, conversation.ID, session.ID, "attention", histClock)
	attentionActivity(t, s, conversation.ID, "attention", "one", domain.ActivityKindUserInput, domain.ActivityStatusPending)
	page, err := s.ConversationAttention(ctx, session.ID, conversation.ID, domain.ConversationAttentionQuery{State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.LatestSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.MarkConversationAttentionRead(ctx, session.ID, conversation.ID, page.ActiveBranchID, "one", page.Items[0].Activity.Revision, histClock); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.EventsAfter(ctx, before, 10)
	if err != nil || len(events) != 1 || events[0].SessionID != string(session.ID) {
		t.Fatalf("seen CDC=%+v, %v", events, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	page, err = s.ConversationAttention(ctx, session.ID, conversation.ID, domain.ConversationAttentionQuery{State: "open"})
	if err != nil || page.Counts.Open != 1 || page.Counts.Unread != 0 || len(page.Items) != 1 || !page.Items[0].Read {
		t.Fatalf("reopened seen state=%+v, %v", page, err)
	}
}
