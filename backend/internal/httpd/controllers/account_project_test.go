package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type projectAccountService struct {
	AgentAccountService
	calls int
	err   error
}

func (s *projectAccountService) SetProjectAccount(ctx context.Context, p, a, role, reviewer string) error {
	s.calls++
	if p != "p" || a != "A" || role != "reviewer" || reviewer != "codex" {
		return ports.ErrAgentAccountScopeInvalid
	}
	return s.err
}
func TestProjectAccountRouteUsesNarrowMutation(t *testing.T) {
	for _, tt := range []struct {
		err    error
		status int
	}{{nil, 204}, {ports.ErrAgentAccountProviderMismatch, 400}, {ports.ErrAgentAccountScopeInvalid, 400}, {ports.ErrAgentAccountProjectUnknown, 404}} {
		svc := &projectAccountService{err: tt.err}
		r := chi.NewRouter()
		(&AgentAccountsController{Svc: svc}).Register(r)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/agents/accounts/A/projects/p", strings.NewReader(`{"role":"reviewer","reviewer":"codex"}`)))
		if w.Code != tt.status || svc.calls != 1 {
			t.Fatalf("status=%d calls=%d body=%s", w.Code, svc.calls, w.Body.String())
		}
	}
}

type operationHandleService struct {
	AgentAccountService
	phase string
}

func (s operationHandleService) Operation(context.Context, string) (domain.AgentAccountOperation, error) {
	return domain.AgentAccountOperation{ID: "op", AccountID: "A", Action: "login", Phase: s.phase, TerminalID: "shellterm-op"}, nil
}
func TestAccountOperationExposesOnlyPendingLoginTerminalHandle(t *testing.T) {
	for _, phase := range []string{"pending", "accepted", "complete"} {
		r := chi.NewRouter()
		(&AgentAccountsController{Svc: operationHandleService{phase: phase}}).Register(r)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/agents/accounts/operations/op", nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), "shellterm-op") != (phase == "pending") {
			t.Fatalf("phase=%s response=%s", phase, w.Body.String())
		}
	}
}
