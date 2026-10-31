package controllers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reviewcore "github.com/aoagents/agent-orchestrator/backend/internal/review"
)

type reviewerSettingsService struct {
	fakeReviewService
	caller ports.AgentSettingsCaller
	change reviewcore.ReviewerSettingsChange
	id     string
}

func (s *reviewerSettingsService) ReviewerAgentSettings(ctx context.Context, id string) (domain.ReviewerAgentSettingsState, error) {
	s.id, s.caller = id, ports.AgentSettingsCallerFromContext(ctx)
	selection := domain.AgentSettings{Model: "review-only", FastMode: domain.AgentFastOff}
	return domain.ReviewerAgentSettingsState{Review: domain.Review{ID: id, Harness: domain.ReviewerCodex}, Settings: &selection, Revision: 3}, nil
}

func (s *reviewerSettingsService) ChangeReviewerAgentSettings(ctx context.Context, id string, req reviewcore.ReviewerSettingsChange) (domain.ReviewerAgentSettingsOperation, error) {
	s.id, s.caller, s.change = id, ports.AgentSettingsCallerFromContext(ctx), req
	return domain.ReviewerAgentSettingsOperation{ID: "operation", Owner: domain.ReviewerSettingsOwner{ReviewID: id, AccountID: "private-account", NativeID: "private-native", Generation: "private-epoch"}, Revision: 4, Phase: "pinned", ApplyMode: "next-pass", Target: domain.AgentSettings{Model: "review-next", FastMode: domain.AgentFastOn}, Authority: domain.AgentSettingsAuthority{ActorGeneration: "private-grant-scope"}, RequestFingerprint: "private-fingerprint", IdempotencyKey: "private-key"}, nil
}

func TestReviewerSettingsHTTPUsesStableOwnerAndPrivateProof(t *testing.T) {
	svc := &reviewerSettingsService{}
	router := chi.NewRouter()
	(&controllers.ReviewsController{Svc: svc}).Register(router)
	r := httptest.NewRequest(http.MethodPatch, "/reviews/stable-review/agent-settings", strings.NewReader(`{"expectedRevision":3,"settings":{"model":"review-next","fastMode":"on"}}`))
	r.Header.Set("Idempotency-Key", "intent-key")
	r.Header.Set("X-AO-Session-ID", "orch")
	r.Header.Set("X-AO-Settings-Grant", "secret-canary")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 200 || svc.id != "stable-review" || svc.change.BaseRevision != 3 || svc.change.IdempotencyKey != "intent-key" || svc.caller.Grant != "secret-canary" {
		t.Fatalf("response=%d %s change=%+v", w.Code, w.Body.String(), svc.change)
	}
	for _, private := range []string{"private-", "secret-canary", "accountId", "nativeId", "authority", "requestFingerprint", "idempotencyKey"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("public response exposed %s: %s", private, w.Body.String())
		}
	}
	if !strings.Contains(w.Body.String(), `"phase":"pinned"`) || !strings.Contains(w.Body.String(), `"applyMode":"next-pass"`) {
		t.Fatalf("pin claimed provider applied: %s", w.Body.String())
	}
}
