package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

type countingProjectManager struct {
	projectsvc.Manager
	calls int
}

func (m *countingProjectManager) project() projectsvc.Project {
	m.calls++
	return projectsvc.Project{ID: "p", Config: &domain.ProjectConfig{AgentRules: "kept"}}
}
func (m *countingProjectManager) Add(context.Context, projectsvc.AddInput) (projectsvc.Project, error) {
	return m.project(), nil
}
func (m *countingProjectManager) Clone(context.Context, projectsvc.CloneInput) (projectsvc.Project, error) {
	return m.project(), nil
}
func (m *countingProjectManager) PrepareClone(context.Context, projectsvc.CloneInput) (projectsvc.ClonePreparationResult, error) {
	m.calls++
	return projectsvc.ClonePreparationResult{}, nil
}
func (m *countingProjectManager) CheckCloneDestination(context.Context, projectsvc.CloneInput) (projectsvc.CloneDestination, error) {
	m.calls++
	return projectsvc.CloneDestination{}, nil
}
func (m *countingProjectManager) UpdateSettings(context.Context, domain.ProjectID, projectsvc.UpdateSettingsInput) (projectsvc.Project, error) {
	return m.project(), nil
}
func (m *countingProjectManager) PatchSettings(context.Context, domain.ProjectID, projectsvc.PatchSettingsInput) (projectsvc.Project, error) {
	return m.project(), nil
}
func (m *countingProjectManager) SetConfig(context.Context, domain.ProjectID, projectsvc.SetConfigInput) (projectsvc.Project, error) {
	return m.project(), nil
}

func TestProjectConfigRoutesRejectRemovedAgentAccountsBeforeService(t *testing.T) {
	clone := `"remoteUrl":"https://example.invalid/r.git","destinationParent":"/fake/parent"`
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/projects", `{"path":"/fake/p","config":{"agentAccounts":{"codex":"P"}}}`},
		{http.MethodPost, "/projects/clone", `{` + clone + `,"config":{"agentAccounts":{}}}`},
		{http.MethodPost, "/projects/clone/prepare", `{` + clone + `,"config":{"agentAccounts":{"codex":"P"}}}`},
		{http.MethodPost, "/projects/clone/prepare/stream", `{` + clone + `,"config":{"agentAccounts":{"codex":"P"}}}`},
		{http.MethodPost, "/projects/clone/check", `{` + clone + `,"config":{"agentAccounts":{"codex":"P"}}}`},
		{http.MethodPut, "/projects/p", `{"displayName":"P","config":{"agentAccounts":null}}`},
		{http.MethodPut, "/projects/p/config", `{"config":{"agentAccounts":{"claude":"P"}}}`},
		{http.MethodPatch, "/projects/p/settings", `{"config":{"agentAccounts":{"codex":"P"}},"expected":{"config":{"agentAccounts":null}}}`},
		{http.MethodPatch, "/projects/p/settings", `{"config":{"agentRules":"x"},"expected":{"config":{"agentAccounts":null}}}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			mgr := &countingProjectManager{}
			r := chi.NewRouter()
			(&ProjectsController{Mgr: mgr}).Register(r)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			var body struct{ Code, Message string }
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusBadRequest || body.Code != "INVALID_PROJECT_CONFIG" || !strings.Contains(body.Message, "agentAccounts") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if mgr.calls != 0 {
				t.Fatalf("service called %d times for a rejected request", mgr.calls)
			}
		})
	}
	mgr := &countingProjectManager{}
	r := chi.NewRouter()
	(&ProjectsController{Mgr: mgr}).Register(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/projects/p/settings", strings.NewReader(`{"config":{"agentRules":"kept"},"expected":{"config":{"agentRules":null}}}`)))
	if w.Code != http.StatusOK || mgr.calls != 1 || strings.Contains(w.Body.String(), "agentAccounts") {
		t.Fatalf("ordinary patch: status=%d calls=%d body=%s", w.Code, mgr.calls, w.Body)
	}
}
