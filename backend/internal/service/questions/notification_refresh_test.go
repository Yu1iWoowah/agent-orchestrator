package questions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type refreshStore struct {
	Store
	mu        sync.Mutex
	revisions map[domain.ProjectID]int64
	calls     map[domain.ProjectID]int
	fail      bool
	blocked   chan struct{}
	release   chan struct{}
	delivered chan struct{}
}

func (s *refreshStore) AttentionSourceRevisions(context.Context) (map[domain.ProjectID]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[domain.ProjectID]int64, len(s.revisions))
	for project, revision := range s.revisions {
		result[project] = revision
	}
	return result, nil
}

func (s *refreshStore) AttentionNotificationSources(ctx context.Context, project domain.ProjectID, _ time.Time) ([]domain.ProjectAttentionItem, error) {
	if s.blocked != nil {
		select {
		case s.blocked <- struct{}{}:
		default:
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[project]++
	if s.fail {
		return nil, errors.New("source offline")
	}
	return make([]domain.ProjectAttentionItem, 130), nil
}

// Legacy methods let the test fail on the previous paged full-scan path.
func (s *refreshStore) ListProjects(context.Context) ([]domain.ProjectRecord, error) {
	return []domain.ProjectRecord{{ID: "a"}, {ID: "b"}}, nil
}

func (s *refreshStore) ProjectAttention(ctx context.Context, project domain.ProjectID, _ domain.ProjectAttentionQuery, at time.Time) (domain.ProjectAttentionPage, error) {
	items, err := s.AttentionNotificationSources(ctx, project, at)
	return domain.ProjectAttentionPage{Items: items}, err
}

func (s *refreshStore) AttentionQuestionExpirationSessions(context.Context, time.Time) ([]domain.SessionID, error) {
	return nil, nil
}

func (s *refreshStore) PendingAttentionAnswers(context.Context, int64) ([]domain.AttentionAnswerDelivery, error) {
	if s.delivered != nil {
		select {
		case s.delivered <- struct{}{}:
		default:
		}
	}
	return nil, nil
}

type refreshNotifications struct{ sizes []int }

func (n *refreshNotifications) ReconcileAttention(_ context.Context, _ domain.ProjectID, items []domain.ProjectAttentionItem) error {
	n.sizes = append(n.sizes, len(items))
	return nil
}

func TestNotificationRefreshOnlyReadsChangedProjectsWithoutPaging(t *testing.T) {
	store := &refreshStore{revisions: map[domain.ProjectID]int64{"a": 1, "b": 2}, calls: map[domain.ProjectID]int{}}
	notifications := &refreshNotifications{}
	svc := New(Deps{Store: store, Notifications: notifications})
	for range 5 {
		if err := svc.RefreshNotifications(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if store.calls["a"] != 1 || store.calls["b"] != 1 || len(notifications.sizes) != 2 || notifications.sizes[0] != 130 {
		t.Fatalf("unchanged sources were scanned again or paged: calls=%v sizes=%v", store.calls, notifications.sizes)
	}
	store.revisions["a"]++
	store.fail = true
	if err := svc.RefreshNotifications(context.Background()); err == nil {
		t.Fatal("source failure was ignored")
	}
	if len(notifications.sizes) != 2 {
		t.Fatal("source failure reconciled an empty snapshot")
	}
	store.fail = false
	if err := svc.RefreshNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.calls["a"] != 3 || store.calls["b"] != 1 || len(notifications.sizes) != 3 {
		t.Fatalf("changed-source retry or unchanged project isolation failed: calls=%v sizes=%v", store.calls, notifications.sizes)
	}
}

func TestNotificationScanCannotDelayAnswerDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &refreshStore{revisions: map[domain.ProjectID]int64{"a": 1}, calls: map[domain.ProjectID]int{}, blocked: make(chan struct{}, 1), release: make(chan struct{}), delivered: make(chan struct{}, 1)}
	svc := New(Deps{Store: store, Notifications: &refreshNotifications{}, Delivery: noDelivery{}})
	done := svc.Start(ctx, nil)
	t.Cleanup(func() { cancel(); close(store.release); <-done })
	select {
	case <-store.blocked:
	case <-time.After(time.Second):
		t.Fatal("notification scan did not start")
	}
	select {
	case <-store.delivered:
	case <-time.After(time.Second):
		t.Fatal("a blocked notification scan delayed the answer delivery loop")
	}
}
