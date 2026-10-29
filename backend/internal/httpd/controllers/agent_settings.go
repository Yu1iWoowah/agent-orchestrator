package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

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

// AgentSettingsControl is the authorized scheduler boundary for HTTP mutations.
type AgentSettingsControl interface {
	Change(context.Context, agentsettings.Caller, domain.SessionID, agentsettings.Change) (domain.AgentSettingsOperation, error)
	Operation(context.Context, agentsettings.Caller, domain.SessionID, string) (domain.AgentSettingsOperation, error)
	Cancel(context.Context, agentsettings.Caller, domain.SessionID, string, int64) (domain.AgentSettingsOperation, error)
	ReadForCaller(context.Context, agentsettings.Caller, domain.SessionID) (agentsettings.Snapshot, error)
}

// AgentSettingsController exposes preferences, capabilities and settings operations.
type AgentSettingsController struct {
	Svc     AgentSettingsReader
	Control AgentSettingsControl
}

// Register uses the same authorized scheduler for typed and legacy mutations.
func (c *AgentSettingsController) Register(r chi.Router) {
	r.Get("/sessions/{sessionId}/agent-settings", c.read)
	r.Patch("/sessions/{sessionId}/agent-settings", c.change)
	r.Get("/sessions/{sessionId}/agent-settings/operations/{operationId}", c.operation)
	r.Post("/sessions/{sessionId}/agent-settings/operations/{operationId}/cancel", c.cancel)
}
func (c *AgentSettingsController) read(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/sessions/{sessionId}/agent-settings")
		return
	}
	var result agentsettings.Snapshot
	var err error
	if c.Control != nil {
		result, err = c.Control.ReadForCaller(r.Context(), settingsCaller(r), domain.SessionID(chi.URLParam(r, "sessionId")))
	} else {
		result, err = c.Svc.Read(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")))
	}
	if errors.Is(err, ports.ErrAgentSettingsNotFound) {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "AGENT_SETTINGS_NOT_FOUND", "Session agent settings not found", nil)
		return
	}
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	result.CurrentTurn = publicTurnAgentSettings(result.CurrentTurn)
	envelope.WriteJSON(w, http.StatusOK, result)
}

func publicTurnAgentSettings(receipt *domain.AgentSettingsTurnSnapshot) *domain.AgentSettingsTurnSnapshot {
	if receipt == nil {
		return nil
	}
	public := receipt.Clone()
	public.Owner = nil
	return &public
}

func decodeAgentSettingsBody(w http.ResponseWriter, r *http.Request, into any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing settings input")
	}
	return nil
}

func settingsCaller(r *http.Request) agentsettings.Caller {
	return agentsettings.Caller{SessionID: domain.SessionID(r.Header.Get("X-AO-Session-ID")), Grant: r.Header.Get("X-AO-Settings-Grant")}
}

func (c *AgentSettingsController) change(w http.ResponseWriter, r *http.Request) {
	if c.Control == nil {
		apispec.NotImplemented(w, r, "PATCH", "/api/v1/sessions/{sessionId}/agent-settings")
		return
	}
	var in ChangeSessionAgentSettingsPayload
	if err := decodeAgentSettingsBody(w, r, &in); err != nil {
		envelope.WriteAPIError(w, r, 400, "bad_request", "AGENT_SETTINGS_INVALID", "Invalid settings request", nil)
		return
	}
	in.IdempotencyKey = r.Header.Get("Idempotency-Key")
	in.RequestID = middleware.GetReqID(r.Context())
	op, err := c.Control.Change(r.Context(), settingsCaller(r), domain.SessionID(chi.URLParam(r, "sessionId")), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	status := http.StatusOK
	if op.Phase == domain.AgentSettingsPending || op.Phase == domain.AgentSettingsRecoveryRequired {
		status = http.StatusAccepted
	}
	envelope.WriteJSON(w, status, AgentSettingsOperationResponse{Operation: op})
}

func (c *AgentSettingsController) operation(w http.ResponseWriter, r *http.Request) {
	if c.Control == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/sessions/{sessionId}/agent-settings/operations/{operationId}")
		return
	}
	op, err := c.Control.Operation(r.Context(), settingsCaller(r), domain.SessionID(chi.URLParam(r, "sessionId")), chi.URLParam(r, "operationId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, AgentSettingsOperationResponse{Operation: op})
}

func (c *AgentSettingsController) cancel(w http.ResponseWriter, r *http.Request) {
	if c.Control == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/agent-settings/operations/{operationId}/cancel")
		return
	}
	var in CancelAgentSettingsOperationPayload
	if err := decodeAgentSettingsBody(w, r, &in); err != nil || in.ExpectedVersion == nil || *in.ExpectedVersion < 0 {
		envelope.WriteAPIError(w, r, 400, "bad_request", "AGENT_SETTINGS_INVALID", "expectedVersion is required", nil)
		return
	}
	op, err := c.Control.Cancel(r.Context(), settingsCaller(r), domain.SessionID(chi.URLParam(r, "sessionId")), chi.URLParam(r, "operationId"), *in.ExpectedVersion)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, AgentSettingsOperationResponse{Operation: op})
}
