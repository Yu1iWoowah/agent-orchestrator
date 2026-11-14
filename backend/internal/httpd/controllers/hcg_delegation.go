package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

// HCGDelegationService is the fixed policy and report service boundary.
type HCGDelegationService interface {
	GetDelegation(context.Context, domain.SessionID) (reporting.DelegationView, error)
	SetDelegationOverride(context.Context, domain.SessionID, *bool) (reporting.DelegationView, error)
	DelegationEmergencyStop(context.Context, bool) error
	DelegationProfiles(context.Context) ([]reporting.DelegationResolvedProfile, error)
	RegisterDelegation(context.Context, reporting.DelegationRegistration) ([]reporting.DelegationResolvedProfile, error)
	ReadDelegation(context.Context, domain.SessionID, reporting.DelegationRequest) (reporting.DelegationReply, error)
}

// HCGDelegationController mounts only the fixed delegation operations.
type HCGDelegationController struct{ Svc HCGDelegationService }

// Register mounts the loopback-only policy and read routes.
func (c HCGDelegationController) Register(r chi.Router) {
	r.Get("/sessions/{sessionId}/hcg-delegation", c.get)
	r.Put("/sessions/{sessionId}/hcg-delegation/override", c.set)
	r.Delete("/sessions/{sessionId}/hcg-delegation/override", c.clear)
	r.Put("/hcg-delegation/emergency-stop", c.stop)
	r.Get("/hcg-delegation/profiles", c.profiles)
	r.Put("/hcg-delegation/profiles", c.register)
	r.Post("/sessions/{sessionId}/hcg-delegation/read", c.read)
}
func delegationBody(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		envelope.WriteError(w, r, apierr.Invalid("INVALID_JSON", "Invalid delegation request body", nil))
		return false
	}
	return true
}
func (c HCGDelegationController) get(w http.ResponseWriter, r *http.Request) {
	out, err := c.Svc.GetDelegation(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")))
	hcgWrite(w, r, HCGDelegationResponse(out), err)
}
func (c HCGDelegationController) set(w http.ResponseWriter, r *http.Request) {
	var in HCGEnabledRequest
	if !delegationBody(w, r, &in) {
		return
	}
	if in.Enabled == nil {
		hcgWrite(w, r, nil, apierr.Invalid("ENABLED_REQUIRED", "enabled must be true or false", nil))
		return
	}
	out, err := c.Svc.SetDelegationOverride(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), in.Enabled)
	hcgWrite(w, r, HCGDelegationResponse(out), err)
}
func (c HCGDelegationController) clear(w http.ResponseWriter, r *http.Request) {
	out, err := c.Svc.SetDelegationOverride(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), nil)
	hcgWrite(w, r, HCGDelegationResponse(out), err)
}
func (c HCGDelegationController) stop(w http.ResponseWriter, r *http.Request) {
	var in HCGEnabledRequest
	if !delegationBody(w, r, &in) {
		return
	}
	if in.Enabled == nil {
		hcgWrite(w, r, nil, apierr.Invalid("ENABLED_REQUIRED", "enabled must be true or false", nil))
		return
	}
	err := c.Svc.DelegationEmergencyStop(r.Context(), *in.Enabled)
	hcgWrite(w, r, HCGStopResponse{Enabled: *in.Enabled}, err)
}
func (c HCGDelegationController) profiles(w http.ResponseWriter, r *http.Request) {
	out, err := c.Svc.DelegationProfiles(r.Context())
	hcgWrite(w, r, HCGDelegationProfilesResponse{Profiles: out}, err)
}
func (c HCGDelegationController) register(w http.ResponseWriter, r *http.Request) {
	var in HCGDelegationRegistrationRequest
	if !delegationBody(w, r, &in) {
		return
	}
	out, err := c.Svc.RegisterDelegation(r.Context(), reporting.DelegationRegistration(in))
	hcgWrite(w, r, HCGDelegationProfilesResponse{Profiles: out}, err)
}
func (c HCGDelegationController) read(w http.ResponseWriter, r *http.Request) {
	var in HCGDelegationReadRequest
	if !delegationBody(w, r, &in) {
		return
	}
	out, err := c.Svc.ReadDelegation(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), reporting.DelegationRequest(in))
	hcgWrite(w, r, HCGDelegationReadResponse(out), err)
}
