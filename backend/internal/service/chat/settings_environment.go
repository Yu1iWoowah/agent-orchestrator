package chat

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (s *Service) SetAgentSettingsGrantEnvironment(issuer ports.AgentSettingsGrantEnvironment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settingsGrants = issuer
}

func (s *Service) settingsLaunchEnvironment(ctx context.Context, cfg StartConfig, generation string, env map[string]string) (map[string]string, error) {
	s.mu.RLock()
	issuer := s.settingsGrants
	s.mu.RUnlock()
	if issuer == nil {
		return env, nil
	}
	clean := make(map[string]string, len(env)+1)
	for key, value := range env {
		if !strings.EqualFold(key, ports.EnvAgentSettingsGrant) {
			clean[key] = value
		}
	}
	clean[ports.EnvAgentSettingsGrant] = ""
	if conversationOwner(cfg).Kind == domain.ConversationOwnerReview {
		return clean, nil
	}
	grant, err := issuer.AgentSettingsGrantEnv(ctx, cfg.SessionID, cfg.Harness, domain.SessionModeChat, generation)
	if err != nil {
		return nil, err
	}
	clean[ports.EnvAgentSettingsGrant] = grant[ports.EnvAgentSettingsGrant]
	return clean, nil
}

func pinnedSettingsGrantEnvironment(initial, prepared map[string]string) map[string]string {
	grant, pinned := initial[ports.EnvAgentSettingsGrant]
	if !pinned {
		return prepared
	}
	clean := make(map[string]string, len(prepared)+1)
	for key, value := range prepared {
		if !strings.EqualFold(key, ports.EnvAgentSettingsGrant) {
			clean[key] = value
		}
	}
	clean[ports.EnvAgentSettingsGrant] = grant
	return clean
}

func redactSettingsGrantError(err error, env map[string]string) error {
	return agentlaunch.RedactError(err, map[string]string{ports.EnvAgentSettingsGrant: env[ports.EnvAgentSettingsGrant]})
}
