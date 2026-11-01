package chat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type guardianProjectionStore struct {
	Store
	scopes         []domain.GuardianReviewScope
	reviews        []domain.GuardianReview
	observedAt     []time.Time
	activities     []domain.ConversationActivity
	observationErr error
}

func (s *guardianProjectionStore) RecordGuardianReview(_ context.Context, scope domain.GuardianReviewScope, r domain.GuardianReview, at time.Time) error {
	s.scopes = append(s.scopes, scope)
	s.reviews = append(s.reviews, r)
	s.observedAt = append(s.observedAt, at)
	return s.observationErr
}
func (s *guardianProjectionStore) ProjectProviderEvent(ctx context.Context, _ string, _ domain.SessionID, _, _, _, _ string, _ time.Time, project func(context.Context) error) (bool, error) {
	return true, project(ctx)
}
func (s *guardianProjectionStore) ProjectReviewProviderEvent(ctx context.Context, _ string, _ domain.SessionID, _, _, _, _, _ string, _ time.Time, project func(context.Context) error) (bool, error) {
	return true, project(ctx)
}
func (s *guardianProjectionStore) UpsertActivity(_ context.Context, _, _ string, a domain.ConversationActivity, _ time.Time) error {
	s.activities = append(s.activities, a)
	return nil
}

type guardianProjectionConversation struct{ ports.ChatConversation }

func (guardianProjectionConversation) ProviderConversationID() string { return "native-root" }

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
		{name: "observation failure preserves activity", harness: domain.HarnessCodex, fail: true, want: 1},
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
			if tc.want == 1 {
				expected := domain.GuardianReviewScope{SessionID: "session", Generation: "generation", AccountID: "account", NativeRootID: "native-root", LaunchedAt: launched}
				if store.scopes[0] != expected || !reflect.DeepEqual(store.reviews[0], *review) || !store.observedAt[0].Equal(now) {
					t.Fatalf("wrong observation scope/fact: %+v", store.scopes[0])
				}
			}
			if len(store.activities) != 1 || store.activities[0].Kind != domain.ActivityKindAutoReview || store.activities[0].Status != domain.ActivityStatusCompleted || store.activities[0].ProviderItemID != event.ProviderItemID {
				t.Fatalf("activity projection lost: %+v", store.activities)
			}
		})
	}
}
