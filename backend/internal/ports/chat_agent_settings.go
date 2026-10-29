package ports

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

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
// preference when an old provider preimage no longer belongs to current inputs.
// It supplies no new recovery owner and must not infer a sticky native value.
type ChatAgentSettingsRecoveryPreparer interface {
	PrepareAgentSettingsRecovery(context.Context, domain.AgentSettings) (domain.AgentSettingsProviderPlan, error)
}
