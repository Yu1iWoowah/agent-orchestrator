package ports

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ChatAgentSettingsReconciliationEvidence qualifies the current attachment.
// A sticky adapter must supply a provider-qualified snapshot; reading setup or
// returning a successful setter acknowledgement cannot implement this contract.
type ChatAgentSettingsReconciliationEvidence interface {
	ReconciliationEvidence(context.Context, domain.AgentSettingsOwner) (AgentSettingsReconciliationEvidence, error)
}

// AgentSettingsReconciliationEvidence separates per-request wire from sticky
// provider state. Per-turn wire does not claim a current provider observation.
type AgentSettingsReconciliationEvidence struct {
	Kind               string
	Owner              domain.AgentSettingsOwner
	ProviderEpoch      string
	SourceRef          string
	Settings           *domain.AgentSettings
	PermissionVerified bool
}

// AgentSettingsCaller is untrusted transport identity. It contains a bearer
// only in memory; durable actor attribution is resolved by the service.
type AgentSettingsCaller struct {
	SessionID domain.SessionID
	Grant     string `json:"-"`
}

type agentSettingsCallerKey struct{}

// WithAgentSettingsCaller preserves transport identity for shared mutation services.
func WithAgentSettingsCaller(ctx context.Context, caller AgentSettingsCaller) context.Context {
	return context.WithValue(ctx, agentSettingsCallerKey{}, caller)
}

// AgentSettingsCallerFromContext reads transport claims without authorizing them.
func AgentSettingsCallerFromContext(ctx context.Context) AgentSettingsCaller {
	caller, _ := ctx.Value(agentSettingsCallerKey{}).(AgentSettingsCaller)
	return caller
}

// ChatAgentSettingsMutator is shared by legacy selection menus and typed HTTP.
type ChatAgentSettingsMutator interface {
	ChangeAgentSettings(context.Context, domain.SessionID, domain.AgentSettingsPatch, ChatAgentSettingsQueue) (domain.AgentSettingsOperation, error)
}

// ChatAgentSettingsQueue is a controller already admitted by the lifecycle
// boundary. Internal menus must not acquire the same lifecycle gate again.
type ChatAgentSettingsQueue interface {
	QueueAgentSettings(context.Context, domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error)
	SupersedeQueuedAgentSettings(context.Context, domain.AgentSettingsOperation, domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error)
}

// EnvAgentSettingsGrant is private launch state, never a response field.
const EnvAgentSettingsGrant = "AO_AGENT_SETTINGS_GRANT"

// AgentSettingsGrantEnvironment issues credentials only for private native launch state.
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

// AgentSettingsRPCFailure preserves a numeric protocol code without publishing
// the provider's unbounded error text in an attempt DTO or log.
type AgentSettingsRPCFailure struct {
	code  int
	cause error
}

// NewAgentSettingsRPCFailure retains a numeric RPC code without exposing its message.
func NewAgentSettingsRPCFailure(code int, cause error) error {
	return &AgentSettingsRPCFailure{code: code, cause: cause}
}
func (e *AgentSettingsRPCFailure) Error() string { return "agent settings native RPC failed" }
func (e *AgentSettingsRPCFailure) Unwrap() error { return e.cause }

// AgentSettingsJSONRPCCode returns the original numeric protocol code.
func (e *AgentSettingsRPCFailure) AgentSettingsJSONRPCCode() int { return e.code }
