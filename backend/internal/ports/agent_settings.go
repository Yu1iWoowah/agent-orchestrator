package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Agent settings persistence errors.
var (
	ErrAgentSettingsNotFound                = errors.New("agent settings not found")
	ErrAgentSettingsConflict                = errors.New("agent settings changed or another operation is pending")
	ErrAgentSettingsInvalid                 = errors.New("agent settings invalid")
	ErrAgentSettingsUnsupported             = errors.New("agent settings unsupported")
	ErrAgentSettingsCapabilitiesUnavailable = errors.New("agent settings capabilities unavailable")
)

// AgentSettingsPreflight validates a new session after its account is bound and
// before any workspace or provider is started. It never refreshes credentials.
type AgentSettingsPreflight interface {
	ValidateSpawn(context.Context, domain.SessionID, domain.AgentSettings, bool) (domain.AgentSettings, error)
}
