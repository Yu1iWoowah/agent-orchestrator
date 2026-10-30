package sessionmanager

import (
	"context"
	"fmt"
	"maps"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementChatPreflight interface {
	replacementResumeLookup
	PreflightReplacementChat(context.Context, domain.AgentHarness, ports.PermissionMode) error
}

// PreflightReplacementTarget validates the ordinary Claude Chat launch path
// before replacement reserves or stops a source. It creates no session/binding.
func (m *Manager) PreflightReplacementTarget(ctx context.Context, project domain.ProjectRecord) (domain.ReplacementTarget, error) {
	var target domain.ReplacementTarget
	if !project.ArchivedAt.IsZero() {
		return target, domain.ErrReplacementConfigChanged
	}
	if err := ctx.Err(); err != nil {
		return target, err
	}
	harness := project.Config.Orchestrator.Harness
	if harness != domain.HarnessClaudeCode {
		return target, ports.ErrChatUnsupported
	}
	chat, ok := m.chat.(replacementChatPreflight)
	if !ok {
		return target, ports.ErrChatUnsupported
	}
	accounts, ok := m.agentAccounts.(interface {
		ports.AgentAccountResolver
		AccountExecutionEnv(context.Context, domain.AgentHarness, string) (map[string]string, error)
	})
	if !ok {
		return target, ports.ErrAgentAccountUnknown
	}
	agent, ok := m.agents.Agent(harness)
	if !ok {
		return target, ErrUnknownHarness
	}
	projectID := domain.ProjectID(project.ID)
	selected, err := accounts.ResolveAccount(ctx, ports.AgentAccountResolutionRequest{Harness: harness, Role: domain.AgentAccountRoleOrchestrator, ProjectConfig: project.Config})
	if err != nil {
		return target, err
	}
	ctx = ports.WithAgentAccount(ctx, selected)
	config, err := m.resolveAgentConfig(ctx, ports.SpawnConfig{ProjectID: projectID, Harness: harness, Kind: domain.KindOrchestrator, RequestedMode: domain.SessionModeChat}, project.Config)
	if err != nil {
		return target, err
	}
	if err := validateSpawnModel(harness, config.Model); err != nil {
		return target, fmt.Errorf("%w: %w", ErrUnsupportedModel, err)
	}
	if err := chat.PreflightReplacementChat(ctx, harness, config.Permissions); err != nil {
		return target, err
	}
	accountID := selected.AccountID
	accountEnv, err := accounts.AccountExecutionEnv(ctx, harness, accountID)
	if err != nil {
		return target, err
	}
	if accountID == "" {
		return target, ports.ErrAgentAccountUnknown
	}
	// Use the chosen account's environment without the session-based resolver:
	// that resolver creates bindings for every configured provider.
	env := maps.Clone(project.Config.Env)
	if env == nil {
		env = map[string]string{}
	}
	for key, value := range accountEnv {
		setProtectedEnv(env, key, value, envKeysCaseInsensitive)
	}
	pinRuntimePermissionEnv(env, config.Permissions)
	m.augmentAgentRuntimeEnv(agent, env)
	rec := domain.SessionRecord{ProjectID: projectID, Harness: harness, Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{WorkspacePath: project.Path}}
	if err := m.preflightLaunchAuth(ctx, agent, rec, env); err != nil {
		return target, err
	}
	return domain.ReplacementTarget{AgentConfig: config, AccountID: accountID}, nil
}
