package agent

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type unavailableDefaultAccount struct{ ports.AgentAccountExecution }

func (unavailableDefaultAccount) ResolveAccount(context.Context, ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountUnknown
}
func TestConfiguredUnavailableDefaultDoesNotFallBackToDeviceReadiness(t *testing.T) {
	svc := newService(nil, nil, nil)
	svc.SetAccountExecution(unavailableDefaultAccount{}, nil)
	observation, handled := svc.savedDefaultAuthentication(context.Background(), "codex")
	if !handled || observation.State != domain.AgentAuthenticationUnknown || observation.Account != nil {
		t.Fatalf("unavailable managed default fell back to device: %+v handled=%v", observation, handled)
	}
}
