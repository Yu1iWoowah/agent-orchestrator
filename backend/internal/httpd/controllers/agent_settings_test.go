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
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
)

type settingsReader struct{}

func (settingsReader) Read(_ context.Context, id domain.SessionID) (agentsettings.Snapshot, error) {
	if id == "missing" {
		return agentsettings.Snapshot{}, ports.ErrAgentSettingsNotFound
	}
	return agentsettings.Snapshot{SessionAgentSettings: domain.SessionAgentSettings{SessionID: id, Revision: 3, Settings: domain.AgentSettings{Model: "model-a", FastMode: domain.AgentFastOff}}, Capabilities: agentsettings.Capabilities{State: agentsettings.CapabilityUnknown, Reason: "account_catalog_unavailable", Models: []agentsettings.ModelCapabilities{}}}, nil
}
func TestAgentSettingsAPIIsReadOnly(t *testing.T) {
	router := chi.NewRouter()
	(&AgentSettingsController{Svc: settingsReader{}}).Register(router)
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/sessions/worker/agent-settings", 200}, {"GET", "/sessions/missing/agent-settings", 404}, {"PATCH", "/sessions/worker/agent-settings", 405}, {"POST", "/sessions/worker/agent-settings", 405}} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if tc.want == http.StatusOK && (!strings.Contains(w.Body.String(), `"fastMode":"off"`) || !strings.Contains(w.Body.String(), `"state":"unknown"`)) {
			t.Fatalf("lost state: %s", w.Body.String())
		}
	}
}
