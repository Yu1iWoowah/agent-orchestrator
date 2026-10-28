package controllers

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
)

// AgentSettingsReader is the read-only settings service boundary.
type AgentSettingsReader interface {
	Read(context.Context, domain.SessionID) (agentsettings.Snapshot, error)
}

// AgentSettingsController exposes saved preferences and capabilities.
type AgentSettingsController struct{ Svc AgentSettingsReader }

// Register deliberately exposes no settings mutation until runtime scheduling
// and orchestrator authorization are implemented in the later settings slices.
func (c *AgentSettingsController) Register(r chi.Router) {
	r.Get("/sessions/{sessionId}/agent-settings", c.read)
}
func (c *AgentSettingsController) read(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/sessions/{sessionId}/agent-settings")
		return
	}
	result, err := c.Svc.Read(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")))
	if errors.Is(err, ports.ErrAgentSettingsNotFound) {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "AGENT_SETTINGS_NOT_FOUND", "Session agent settings not found", nil)
		return
	}
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, result)
}
