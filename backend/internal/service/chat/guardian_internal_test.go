package chat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type guardianProjectionStore struct {
	queueFailures   int
	queueErr        error
	projectionCalls int
	declineQueue    bool
	Store
	scopes         []domain.GuardianReviewScope
	queuedScopes   []domain.GuardianReviewScope
	queuedReviews  []domain.GuardianReview
	queuedAt       []time.Time
	reviews        []domain.GuardianReview
	observedAt     []time.Time
	activities     []domain.ConversationActivity
	observationErr error
	activityErr    error
	failuresLeft   int
}

func (s *guardianProjectionStore) QueueGuardianReview(_ context.Context, scope domain.GuardianReviewScope, r domain.GuardianReview, at time.Time) (bool, error) {
	if s.queueErr != nil {
		return false, s.queueErr
	}
	if s.queueFailures > 0 {
		s.queueFailures--
		return false, errors.New("private transient sentinel")
	}
	s.queuedScopes = append(s.queuedScopes, scope)
	s.queuedReviews = append(s.queuedReviews, r)
	s.queuedAt = append(s.queuedAt, at)
	return !s.declineQueue, nil
}

func (s *guardianProjectionStore) RecordGuardianReview(_ context.Context, scope domain.GuardianReviewScope, r domain.GuardianReview, at time.Time) error {
	s.scopes = append(s.scopes, scope)
	s.reviews = append(s.reviews, r)
	s.observedAt = append(s.observedAt, at)
	if s.failuresLeft > 0 {
		s.failuresLeft--
		return errors.New("private transient sentinel")
	}
	return s.observationErr
}
func (s *guardianProjectionStore) ProjectProviderEvent(ctx context.Context, _ string, _ domain.SessionID, _, _, _, _ string, _ time.Time, project func(context.Context) error) (bool, error) {
	s.projectionCalls++
	return true, project(ctx)
}
func (s *guardianProjectionStore) ProjectReviewProviderEvent(ctx context.Context, _ string, _ domain.SessionID, _, _, _, _, _ string, _ time.Time, project func(context.Context) error) (bool, error) {
	return true, project(ctx)
}
func (s *guardianProjectionStore) UpsertActivity(_ context.Context, _, _ string, a domain.ConversationActivity, _ time.Time) error {
	if s.activityErr != nil {
		return s.activityErr
	}
	s.activities = append(s.activities, a)
	return nil
}

type guardianProjectionConversation struct {
	ports.ChatConversation
	closes *int
}

func (guardianProjectionConversation) ProviderConversationID() string { return "native-root" }
func (c guardianProjectionConversation) Close() error {
	if c.closes != nil {
		(*c.closes)++
	}
	return nil
}

func TestGuardianReviewProjectUnitScopeGatesAndFailureIsolation(t *testing.T) {
	launched := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                    string
		harness                 domain.AgentHarness
		reviewer, fail, missing bool
		want                    int
	}{
		{name: "Codex root controller", harness: domain.HarnessCodex, want: 1},
		{name: "other harness", harness: domain.HarnessClaudeCode},
		{name: "reviewer owner", harness: domain.HarnessCodex, reviewer: true},
		{name: "observation failure preserves activity", harness: domain.HarnessCodex, fail: true, want: 3},
		{name: "unrelated event", harness: domain.HarnessCodex, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &guardianProjectionStore{}
			if tc.fail {
				store.observationErr = errors.New("synthetic observation failure")
			}
			owner := domain.SessionConversationOwner("session")
			if tc.reviewer {
				owner = domain.ReviewConversationOwner("reviewer")
			}
			now := launched
			c := newController("session", owner, domain.ConversationRecord{ID: "conversation"}, "generation", tc.harness, guardianProjectionConversation{}, store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), func() string { return "activity" }, func() time.Time { return now }, nil, nil, nil)
			c.settingsObservationOwner = c.observationOwner("account")
			now = now.Add(time.Minute)
			review := &domain.GuardianReview{ReviewID: "review", ParentThreadID: "child", ParentTurnID: "turn", Action: "execve", Status: "approved", StartedAt: &launched, SawCompleted: true}
			event := ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "turn", ProviderItemID: "auto-review:review", ActivityKind: domain.ActivityKindAutoReview, ActivityStatus: domain.ActivityStatusCompleted, Summary: "Review completed", GuardianReview: review}
			if tc.missing {
				event.GuardianReview = nil
			}
			if !c.projectUnit(context.Background(), []ports.ChatEvent{event}, nil, false) {
				t.Fatal("projection stream stopped")
			}
			if len(store.scopes) != tc.want {
				t.Fatalf("observation calls = %d, want %d", len(store.scopes), tc.want)
			}
			wantQueued := 0
			if tc.want > 0 {
				wantQueued = 1
			}
			if len(store.queuedScopes) != wantQueued {
				t.Fatalf("queued calls = %d, want %d", len(store.queuedScopes), wantQueued)
			}
			if tc.want > 0 {
				expected := domain.GuardianReviewScope{SessionID: "session", Generation: "generation", AccountID: "account", NativeRootID: "native-root", LaunchedAt: launched}
				if store.queuedScopes[0] != expected || !reflect.DeepEqual(store.queuedReviews[0], *review) || !store.queuedAt[0].Equal(now) || store.scopes[0] != expected || !reflect.DeepEqual(store.reviews[0], *review) || !store.observedAt[0].Equal(now) {
					t.Fatalf("wrong observation scope/fact: %+v", store.scopes[0])
				}
			}
			if len(store.activities) != 1 || store.activities[0].Kind != domain.ActivityKindAutoReview || store.activities[0].Status != domain.ActivityStatusCompleted || store.activities[0].ProviderItemID != event.ProviderItemID {
				t.Fatalf("activity projection lost: %+v", store.activities)
			}
		})
	}
}

func TestGuardianReviewTransientFailureRetriedWithoutAck(t *testing.T) {
	for _, phase := range []string{"capture", "apply", "exhausted", "ignored", "ignored projection failure"} {
		t.Run(phase, func(t *testing.T) {
			now := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
			store := &guardianProjectionStore{}
			switch phase {
			case "capture":
				store.queueFailures = 1
			case "apply":
				store.failuresLeft = 1
			case "exhausted":
				store.observationErr = errors.New("private transient sentinel")
			case "ignored projection failure":
				store.declineQueue = true
				store.activityErr = errors.New("synthetic activity projection failure")
			default:
				store.declineQueue = true
			}
			var logs bytes.Buffer
			c := newController("session", domain.SessionConversationOwner("session"), domain.ConversationRecord{ID: "conversation"}, "generation", domain.HarnessCodex, guardianProjectionConversation{}, store, nil, slog.New(slog.NewTextHandler(&logs, nil)), func() string { return "activity" }, func() time.Time { return now }, nil, nil, nil)
			c.settingsObservationOwner = c.observationOwner("account")
			r := &domain.GuardianReview{ReviewID: "review", ParentThreadID: "native-root", ParentTurnID: "turn", Action: "execve", Status: "approved", StartedAt: &now, SawCompleted: true}
			event := ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "turn", ProviderItemID: "auto-review:review", ActivityKind: domain.ActivityKindAutoReview, ActivityStatus: domain.ActivityStatusCompleted, GuardianReview: r}
			if !c.projectUnit(context.Background(), []ports.ChatEvent{event}, nil, false) {
				t.Fatal("stream stopped")
			}
			wantCalls := 1
			switch phase {
			case "apply":
				wantCalls = 2
			case "exhausted":
				wantCalls = 3
			case "ignored", "ignored projection failure":
				wantCalls = 0
			}
			if len(store.scopes) != wantCalls || store.failuresLeft != 0 || store.queueFailures != 0 {
				t.Fatalf("transient observation calls = %d, want %d (retry succeeds)", len(store.scopes), wantCalls)
			}
			wantActivities := 1
			if phase == "ignored projection failure" {
				wantActivities = 0
			}
			if len(store.activities) != wantActivities {
				t.Fatal("activity missing or duplicated")
			}
			if strings.Contains(logs.String(), "private transient sentinel") {
				t.Fatal("error content leaked")
			}
		})
	}
}

func TestGuardianFailuresNeverStopNonPersistentChat(t *testing.T) {
	for _, phase := range []string{"capture", "activity"} {
		t.Run(phase, func(t *testing.T) {
			now := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
			store := &guardianProjectionStore{}
			if phase == "capture" {
				store.queueErr = errors.New("private capture error sentinel")
			} else {
				store.activityErr = errors.New("synthetic activity failure")
			}
			closes := 0
			var logs bytes.Buffer
			c := newController("session", domain.SessionConversationOwner("session"), domain.ConversationRecord{ID: "conversation"}, "generation", domain.HarnessCodex, guardianProjectionConversation{closes: &closes}, store, nil, slog.New(slog.NewTextHandler(&logs, nil)), func() string { return "activity" }, func() time.Time { return now }, nil, nil, nil)
			c.settingsObservationOwner = c.observationOwner("account")
			initialState := c.state
			r := &domain.GuardianReview{ReviewID: "review", ParentThreadID: "native-root", ParentTurnID: "turn", Action: "execve", Status: "approved", StartedAt: &now, SawCompleted: true}
			event := ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "turn", ProviderItemID: "auto-review", ActivityKind: domain.ActivityKindAutoReview, ActivityStatus: domain.ActivityStatusCompleted, GuardianReview: r}
			if !c.projectUnit(context.Background(), []ports.ChatEvent{event}, nil, false) {
				t.Error("projectUnit returned false")
			}
			if closes != 0 || c.state != initialState {
				t.Errorf("conversation closed=%d, state=%s, want unchanged %s", closes, c.state, initialState)
			}
			wantActivities := 0
			if phase == "capture" {
				wantActivities = 1
			}
			if len(store.activities) != wantActivities {
				t.Errorf("activities=%d, want %d", len(store.activities), wantActivities)
			}
			if len(store.scopes) != 0 {
				t.Error("applied an uncommitted observation")
			}
			if store.projectionCalls != 1 {
				t.Errorf("non-persistent projections=%d, want 1", store.projectionCalls)
			}
			if phase == "capture" && (!strings.Contains(logs.String(), "collection_failed") || strings.Contains(logs.String(), "private capture error sentinel")) {
				t.Error("capture failure log must contain only a fixed code")
			}
			// The next ordinary activity can still be projected on this conversation.
			store.activityErr = nil
			event.GuardianReview = nil
			event.ProviderItemID = "ordinary"
			if !c.projectUnit(context.Background(), []ports.ChatEvent{event}, nil, false) || closes != 0 {
				t.Error("following event cannot continue")
			}
		})
	}
}
