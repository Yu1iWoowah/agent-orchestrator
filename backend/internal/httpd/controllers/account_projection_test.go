package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

func TestRemoteProjectProjectionOmitsLocalAccountReferencesAndSecrets(t *testing.T) {
	config := domain.ProjectConfig{AgentAccounts: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderCodex: "private-A"}, Worker: domain.RoleOverride{AgentAccountID: "private-A"}, Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerCodex, AgentAccountID: "private-C"}}, Env: map[string]string{"CODEX_HOME": "/fake/private/profile", "OPENAI_API_KEY": "fake-credential-marker", "KEEP": "ordinary"}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p", nil)
	req = req.WithContext(ports.WithRemoteAccountRequest(req.Context()))
	projected := projectForAccountRequest(req, projectsvc.Project{ID: "p", Config: &config})
	raw, err := json.Marshal(ProjectResponse{Project: projected})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-A", "private-C", "/fake/private/profile", "fake-credential-marker"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("remote export leaked %s", secret)
		}
	}
	if projected.Config.Env["KEEP"] != "ordinary" || config.Worker.AgentAccountID != "private-A" {
		t.Fatal("projection changed local data or unrelated env")
	}
}
