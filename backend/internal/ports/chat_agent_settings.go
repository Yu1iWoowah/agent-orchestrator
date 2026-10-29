package ports

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// AgentSettingsCaller is untrusted transport identity. It contains a bearer
// only in memory; durable actor attribution is resolved by the service.
type AgentSettingsCaller struct {
	SessionID domain.SessionID
	Grant     string `json:"-"`
}

type agentSettingsCallerKey struct{}

func WithAgentSettingsCaller(ctx context.Context, caller AgentSettingsCaller) context.Context {
	return context.WithValue(ctx, agentSettingsCallerKey{}, caller)
}

func AgentSettingsCallerFromContext(ctx context.Context) AgentSettingsCaller {
	caller, _ := ctx.Value(agentSettingsCallerKey{}).(AgentSettingsCaller)
	return caller
}

// ChatAgentSettingsMutator is shared by legacy selection menus and typed HTTP.
type ChatAgentSettingsMutator interface {
	ChangeAgentSettings(context.Context, domain.SessionID, domain.AgentSettingsPatch) (domain.AgentSettingsOperation, error)
}

// EnvAgentSettingsGrant is private launch state, never a response field.
const EnvAgentSettingsGrant = "AO_AGENT_SETTINGS_GRANT"

type AgentSettingsGrantEnvironment interface {
	AgentSettingsGrantEnv(context.Context, domain.SessionID, domain.AgentHarness, domain.SessionMode, string) (map[string]string, error)
}

// ChatAgentSettingsController stages an internal, journalled settings operation.
// Preparation is read-only. The caller owns the dispatch gate, persists the
// preimage and checkpoints before setters, and qualifies the provider identity.
type ChatAgentSettingsController interface {
	PrepareAgentSettings(context.Context, domain.AgentSettings, domain.AgentSettings) (domain.AgentSettingsProviderPlan, error)
	ApplyAgentSettingsStep(context.Context, domain.AgentSettingsProviderPlan, domain.AgentSettingsCheckpoint, bool) error
}

// ChatAgentSettingsValidator is supplied by the authorized service boundary.
// The executor never supplies its own policy approval. Restoring validates the
// current durable preference rather than authorizing the failed target again.
type ChatAgentSettingsValidator interface {
	ValidateAgentSettings(context.Context, domain.AgentSettingsOperation, domain.AgentSettingsOwner, bool) (domain.AgentSettingsQualification, error)
}

// ChatAgentSettingsRecoveryPreparer can prove fresh wire for the durable
// preference on cold replay or when an old preimage no longer belongs to
// current inputs.
// It supplies no new recovery owner and must not infer a sticky native value.
type ChatAgentSettingsRecoveryPreparer interface {
	PrepareAgentSettingsRecovery(context.Context, domain.AgentSettings) (domain.AgentSettingsProviderPlan, error)
}
