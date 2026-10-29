package codex

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/codexappserver"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// PrepareTUIAgentSettings reads config/model metadata from a separate app-server
// connection. No native thread or inference turn is opened by this probe.
func (p *Plugin) PrepareTUIAgentSettings(ctx context.Context, cfg ports.RestoreConfig, source, target domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	binary, err := p.codexBinary(ctx)
	if err != nil {
		return domain.AgentSettingsProviderPlan{}, err
	}
	plan, err := codexappserver.DiscoverAgentSettingsPlan(ctx, binary, cfg.Session.WorkspacePath, cfg.Env, source, target)
	if err != nil {
		return domain.AgentSettingsProviderPlan{}, err
	}
	plan.Protocol = "codex-native-resume"
	plan.ProviderConversationID = cfg.Session.Metadata[ports.MetadataKeyAgentSessionID]
	plan.Permission = string(cfg.Permissions)
	return plan, nil
}
