package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestDisabledGuardianReviewDoesNotWaitForProjectionWriter(t *testing.T) {
	st, session, conversation := conversationFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		_, err := st.ProjectProviderEvent(context.Background(), conversation, session, "gen-1", "held-event", "test.held", "{}", histClock, func(context.Context) error { close(entered); <-release; return nil })
		writerDone <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reviewDone := make(chan error, 1)
	scope := domain.GuardianReviewScope{SessionID: session, Generation: "gen-1", AccountID: "account", NativeRootID: "root", LaunchedAt: histClock}
	review := domain.GuardianReview{ReviewID: "review", ParentThreadID: "root", ParentTurnID: "turn", Action: "command", Status: "inProgress", StartedAt: &histClock, SawStarted: true}
	go func() { reviewDone <- st.RecordGuardianReview(ctx, scope, review, histClock) }()
	var result error
	blocked := false
	select {
	case result = <-reviewDone:
	case <-ctx.Done():
		blocked = true
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if blocked {
		<-reviewDone
		t.Fatal("disabled observation waited for the projection writer")
	}
	if result != nil {
		t.Fatal(result)
	}
}

func TestEmptyGuardianReplayDoesNotWaitForProjectionWriter(t *testing.T) {
	st, session, conversation := conversationFixture(t)
	_, err := st.UpdateProjectSettings(context.Background(), "hist", "", domain.ProjectConfig{GuardianObservability: domain.GuardianObservabilityConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		_, err := st.ProjectProviderEvent(context.Background(), conversation, session, "gen-1", "held-replay", "test.held", "{}", histClock, func(context.Context) error { close(entered); <-release; return nil })
		writerDone <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	replayDone := make(chan error, 1)
	go func() { replayDone <- st.ReplayGuardianReviews(ctx, histClock) }()
	blocked := false
	var replayErr error
	select {
	case replayErr = <-replayDone:
	case <-ctx.Done():
		blocked = true
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if blocked {
		<-replayDone
		t.Fatal("empty replay waited for the projection writer")
	}
	if replayErr != nil {
		t.Fatal(replayErr)
	}
}
