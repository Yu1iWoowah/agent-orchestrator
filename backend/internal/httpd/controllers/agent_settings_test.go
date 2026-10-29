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
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
)

type settingsReader struct{}

func (settingsReader) Read(_ context.Context, id domain.SessionID) (agentsettings.Snapshot, error) {
	if id == "missing" {
		return agentsettings.Snapshot{}, ports.ErrAgentSettingsNotFound
	}
	result := agentsettings.Snapshot{SessionAgentSettings: domain.SessionAgentSettings{SessionID: id, Revision: 3, Settings: domain.AgentSettings{Model: "model-a", FastMode: domain.AgentFastOff}}, Effective: domain.UnknownAgentSettingsEffective("provider_not_reported"), Capabilities: agentsettings.Capabilities{State: agentsettings.CapabilityUnknown, Reason: "account_catalog_unavailable", Models: []agentsettings.ModelCapabilities{}}}
	if id == "not-offered" {
		result.Settings.FastMode = domain.AgentFastOn
		result.Effective.FastMode = domain.AgentSettingObservation{State: domain.AgentSettingNotProvided, Reason: "provider_not_offered"}
	}
	return result, nil
}

func TestAgentSettingsReadKeepsRequestedAndEffectiveSeparate(t *testing.T) {
	router := chi.NewRouter()
	(&AgentSettingsController{Svc: settingsReader{}}).Register(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions/not-offered/agent-settings", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Settings  domain.AgentSettings          `json:"settings"`
		Effective domain.AgentSettingsEffective `json:"effective"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Settings.FastMode != domain.AgentFastOn || got.Effective.FastMode.State != domain.AgentSettingNotProvided || got.Effective.FastMode.Value != "" {
		t.Fatalf("requested was reported as effective: %s", w.Body.String())
	}
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
