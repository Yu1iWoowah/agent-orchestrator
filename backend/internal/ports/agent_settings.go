package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Agent settings persistence errors.
var (
	ErrAgentSettingsNativeHistoryUnavailable = errors.New("agent settings native history unavailable")
	ErrAgentSettingsNotFound                 = errors.New("agent settings not found")
	ErrAgentSettingsConflict                 = errors.New("agent settings changed or another operation is pending")
	ErrAgentSettingsInvalid                  = errors.New("agent settings invalid")
	ErrAgentSettingsUnsupported              = errors.New("agent settings unsupported")
	ErrAgentSettingsCapabilitiesUnavailable  = errors.New("agent settings capabilities unavailable")
	ErrAgentSettingsGrantInvalid             = errors.New("agent settings grant invalid")
	ErrAgentSettingsGrantExpired             = errors.New("agent settings grant expired")
)

// AgentSettingsPreflight validates a new session after its account is bound and
// before any workspace or provider is started. It never refreshes credentials.
type AgentSettingsPreflight interface {
	ValidateSpawn(context.Context, domain.SessionID, domain.AgentSettings, bool) (domain.AgentSettings, error)
}

// AgentTUISettingsPreparer proves session-local resume wire without starting a
// provider conversation or writing profile settings. Empty native fields are
// not reset instructions; the provider must supply its reset protocol.
type AgentTUISettingsPreparer interface {
	PrepareTUIAgentSettings(context.Context, RestoreConfig, domain.AgentSettings, domain.AgentSettings) (domain.AgentSettingsProviderPlan, error)
}
