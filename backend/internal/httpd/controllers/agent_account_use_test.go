package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/go-chi/chi/v5"
)

type rebuildReviewerService struct{ AgentAccountService }

func (rebuildReviewerService) RebuildReviewer(_ context.Context, id, target string) (domain.Review, error) {
	if id != "old-review" || target != "account-B" {
		return domain.Review{}, ports.ErrAgentAccountUnknown
	}
	return domain.Review{ID: "fresh-review", SessionID: "fake-worker", Harness: domain.ReviewerCodex}, nil
}

func TestReviewerAccountRebuildIsAnExplicitFreshExecution(t *testing.T) {
	router := chi.NewRouter()
	(&AgentAccountsController{Svc: rebuildReviewerService{}}).Register(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/agents/accounts/reviewers/old-review/rebuild", strings.NewReader(`{"accountId":"account-B"}`)))
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), "fresh-review") || !strings.Contains(w.Body.String(), "account-B") {
		t.Fatalf("rebuild route: %d %s", w.Code, w.Body.String())
	}
}

type usageOwnersService struct{ AgentAccountService }

func (usageOwnersService) UsageOwners(context.Context, string) ([]domain.AgentAccountUsageOwner, error) {
	return []domain.AgentAccountUsageOwner{{Kind: "reviewer", SessionID: "fake-worker", SessionName: "Fake review source", ReviewID: "opaque-review", PRURL: "https://github.com/fake/repo/pull/1", Harness: domain.HarnessCodex, Blocking: true}}, nil
}

func TestAccountUsageOwnersRouteAndRefusedRemovalDescribeTheOwner(t *testing.T) {
	router := chi.NewRouter()
	(&AgentAccountsController{Svc: usageOwnersService{}}).Register(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/agents/accounts/account-A/usage-owners", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Fake review source") || !strings.Contains(w.Body.String(), "https://github.com/fake/repo/pull/1") {
		t.Fatalf("owner route: %d %s", w.Code, w.Body.String())
	}
	owners, _ := usageOwnersService{}.UsageOwners(context.Background(), "account-A")
	w = httptest.NewRecorder()
	writeAgentAccountError(w, httptest.NewRequest(http.MethodDelete, "/agents/accounts/account-A", nil), &ports.AgentAccountInUseError{Owners: owners})
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "cleanup is scheduled") || !strings.Contains(w.Body.String(), `"scheduled":false`) || !strings.Contains(w.Body.String(), "Fake review source") {
		t.Fatalf("refused removal was presented as scheduled: %d %s", w.Code, w.Body.String())
	}
}
