package ports

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ChatAgentSettingsObserver reads cached provider receipts without setters,
// network requests, process startup, or preference projection.
type ChatAgentSettingsObserver interface {
	AgentSettingsObservation() domain.AgentSettingsEffective
}

// AgentSettingsObservationSource reads an existing controller without waking it.
type AgentSettingsObservationSource interface {
	ObserveAgentSettings(context.Context, domain.SessionID) (domain.AgentSettingsObservation, error)
}
