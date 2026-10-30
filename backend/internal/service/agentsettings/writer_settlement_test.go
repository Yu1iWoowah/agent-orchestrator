package agentsettings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

type operatorSettlementRuntime struct {
	TUISettingsRuntime
	calls int
	err   error
}

func (s *operatorSettlementRuntime) SettleTUIAgentSettings(_ context.Context, id domain.SessionID, operation string, version int64, stop bool) (domain.AgentSettingsOperation, error) {
	s.calls++
	return domain.AgentSettingsOperation{ID: operation, SessionID: id, Version: version + 1, Phase: domain.AgentSettingsCancelled, TUI: &domain.AgentSettingsTUIHandoff{TerminationVerified: true}}, s.err
}

func TestSettingsWriterSettlementControlRequiresLocalOperator(t *testing.T) {
	for _, caller := range []Caller{{}, {SessionID: "orchestrator"}, {Grant: "grant"}, {SessionID: "orchestrator", Grant: "grant"}} {
		runtime := &operatorSettlementRuntime{}
		control := NewControl(nil, nil, nil, nil, time.Now)
		control.SetTUIRuntime(runtime)
		boundary, ok := any(control).(interface {
			SettleWriter(context.Context, Caller, domain.SessionID, string, int64, bool) (domain.AgentSettingsOperation, error)
		})
		if !ok {
			t.Fatal("local operator settlement control missing")
		}
		_, err := boundary.SettleWriter(t.Context(), caller, "worker", "op", 7, true)
		if caller == (Caller{}) {
			if err != nil || runtime.calls != 1 {
				t.Fatalf("local cleanup rejected: %v calls=%d", err, runtime.calls)
			}
		} else {
			var structured *apierr.Error
			if !errors.As(err, &structured) || structured.Code != "AGENT_SETTINGS_OPERATOR_REQUIRED" || runtime.calls != 0 {
				t.Fatalf("agent must not settle a native writer: %v calls=%d", err, runtime.calls)
			}
		}
		runtime.err = errors.New("native diagnostics contain injected-grant-canary")
		_, err = boundary.SettleWriter(t.Context(), Caller{}, "worker", "op", 7, false)
		var structured *apierr.Error
		if !errors.As(err, &structured) || structured.Code != "AGENT_SETTINGS_FAILED" || strings.Contains(err.Error(), "injected-grant-canary") {
			t.Fatalf("operator recovery leaked native diagnostics: %v", err)
		}
	}
}
