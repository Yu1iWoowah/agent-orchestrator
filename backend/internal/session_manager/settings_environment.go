package sessionmanager

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SetAgentSettingsGrantEnvironment completes daemon-only startup wiring.
func (m *Manager) SetAgentSettingsGrantEnvironment(issuer ports.AgentSettingsGrantEnvironment) {
	m.settingsGrants = issuer
}

func redactSettingsGrantError(err error, env map[string]string) error {
	return agentlaunch.RedactError(err, map[string]string{ports.EnvAgentSettingsGrant: env[ports.EnvAgentSettingsGrant]})
}

func (m *Manager) injectSettingsGrant(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, generation string, env map[string]string) error {
	setProtectedEnv(env, ports.EnvAgentSettingsGrant, "", envKeysCaseInsensitive)
	if m.settingsGrants == nil {
		return nil
	}
	grant, err := m.settingsGrants.AgentSettingsGrantEnv(ctx, id, harness, domain.SessionModeTUI, generation)
	if err != nil {
		return err
	}
	env[ports.EnvAgentSettingsGrant] = grant[ports.EnvAgentSettingsGrant]
	return nil
}
