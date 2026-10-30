package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	"github.com/go-chi/chi/v5"
)

type writerSettlementHTTPControl struct {
	*settingsControls
	version int64
	stop    bool
	calls   int
}

func (c *writerSettlementHTTPControl) SettleWriter(_ context.Context, _ agentsettings.Caller, id domain.SessionID, operationID string, version int64, stop bool) (domain.AgentSettingsOperation, error) {
	c.calls++
	c.version, c.stop = version, stop
	return domain.AgentSettingsOperation{ID: operationID, SessionID: id, Phase: domain.AgentSettingsCancelled, Version: version + 1, TUI: &domain.AgentSettingsTUIHandoff{TerminationVerified: true}}, nil
}

func TestAgentSettingsWriterSettlementHTTPRequiresVersionAndNoClaimedProof(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"expectedVersion":7,"stopIfAlive":true}`, 200},
		{`{}`, 400},
		{`{"expectedVersion":-1}`, 400},
		{`{"expectedVersion":7,"terminationVerified":true}`, 400},
	} {
		control := &writerSettlementHTTPControl{settingsControls: &settingsControls{}}
		router := chi.NewRouter()
		(&AgentSettingsController{Control: control}).Register(router)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sessions/worker/agent-settings/operations/op/settle", strings.NewReader(tc.body)))
		if recorder.Code != tc.status {
			t.Fatalf("settlement status=%d want=%d body=%s", recorder.Code, tc.status, recorder.Body.String())
		}
		if tc.status == 200 {
			if control.calls != 1 || control.version != 7 || !control.stop || !strings.Contains(recorder.Body.String(), `"writerStatus":"stopped"`) || strings.Contains(recorder.Body.String(), "RuntimeHandle") {
				t.Fatalf("invalid public settlement: calls=%d body=%s", control.calls, recorder.Body.String())
			}
		} else if control.calls != 0 {
			t.Fatal("invalid caller body reached runtime")
		}
	}
}
