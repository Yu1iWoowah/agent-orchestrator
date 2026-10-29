package codexappserver

import (
	"context"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// DiscoverAgentSettingsPlan proves fresh baseline reset wire with config/read
// and model/list. Like DiscoverModels it opens no thread and sends no turn.
func DiscoverAgentSettingsPlan(ctx context.Context, binary, workdir string, env map[string]string, source, target domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	driver := New(fixedCodexPlugin(binary), slog.New(slog.DiscardHandler))
	return driver.DiscoverAgentSettingsPlan(ctx, workdir, env, source, target)
}

// DiscoverAgentSettingsPlan uses this driver's bounded read-only connection.
func (driver *Driver) DiscoverAgentSettingsPlan(ctx context.Context, workdir string, env map[string]string, source, target domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	conv, err := driver.connect(ctx, workdir, env, "")
	if err != nil {
		return domain.AgentSettingsProviderPlan{}, err
	}
	defer func() { _ = conv.Close() }()
	return conv.PrepareAgentSettings(ctx, source, target)
}
