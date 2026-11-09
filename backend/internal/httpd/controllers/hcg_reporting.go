package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

// HCGReportingService is the fixed consumer boundary; callers cannot supply source paths.
type HCGReportingService interface {
	Get(context.Context, domain.SessionID) (reporting.View, error)
	SetOverride(context.Context, domain.SessionID, *bool) (reporting.View, error)
	EmergencyStop(context.Context, bool) error
	Register(context.Context, string, string) (reporting.EngineStatus, error)
	Report(context.Context, domain.SessionID) (reporting.Snapshot, error)
}

// HCGReportingController serves native-loopback policy and safe report operations.
type HCGReportingController struct{ Svc HCGReportingService }

// Register is mounted only behind the native loopback control gate.
func (c HCGReportingController) Register(r chi.Router) {
	r.Get("/sessions/{sessionId}/hcg-reporting", c.get)
	r.Put("/sessions/{sessionId}/hcg-reporting/override", c.set)
	r.Delete("/sessions/{sessionId}/hcg-reporting/override", c.clear)
	r.Put("/hcg-reporting/emergency-stop", c.stop)
	r.Put("/hcg-reporting/engine", c.register)
	r.Post("/usage/sessions/{sessionId}/report", c.report)
}

func hcgBody(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		envelope.WriteError(w, r, apierr.Invalid("INVALID_JSON", "Invalid reporting request body", nil))
		return false
	}
	return true
}

func hcgWrite(w http.ResponseWriter, r *http.Request, result any, err error) {
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, result)
}

func (c HCGReportingController) get(w http.ResponseWriter, r *http.Request) {
	v, err := c.Svc.Get(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")))
	hcgWrite(w, r, HCGReportingResponse(v), err)
}

func (c HCGReportingController) set(w http.ResponseWriter, r *http.Request) {
	var in HCGEnabledRequest
	if !hcgBody(w, r, &in) {
		return
	}
	if in.Enabled == nil {
		hcgWrite(w, r, nil, apierr.Invalid("ENABLED_REQUIRED", "enabled must be true or false", nil))
		return
	}
	v, err := c.Svc.SetOverride(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), in.Enabled)
	hcgWrite(w, r, HCGReportingResponse(v), err)
}

func (c HCGReportingController) clear(w http.ResponseWriter, r *http.Request) {
	v, err := c.Svc.SetOverride(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), nil)
	hcgWrite(w, r, HCGReportingResponse(v), err)
}

func (c HCGReportingController) stop(w http.ResponseWriter, r *http.Request) {
	var in HCGEnabledRequest
	if !hcgBody(w, r, &in) {
		return
	}
	if in.Enabled == nil {
		hcgWrite(w, r, nil, apierr.Invalid("ENABLED_REQUIRED", "enabled must be true or false", nil))
		return
	}
	err := c.Svc.EmergencyStop(r.Context(), *in.Enabled)
	hcgWrite(w, r, HCGStopResponse{Enabled: *in.Enabled}, err)
}

func (c HCGReportingController) register(w http.ResponseWriter, r *http.Request) {
	var in HCGEngineRequest
	if !hcgBody(w, r, &in) {
		return
	}
	if in.Engine != reporting.EngineName || in.ExpectedVersion != reporting.EngineVersion || !filepath.IsAbs(in.Executable) {
		hcgWrite(w, r, nil, apierr.Invalid("INVALID_REPORT_ENGINE", "A fixed llm-usage-metrics 0.14.3 absolute executable is required", nil))
		return
	}
	v, err := c.Svc.Register(r.Context(), in.Executable, in.ExpectedVersion)
	hcgWrite(w, r, HCGEngineResponse(v), err)
}

func (c HCGReportingController) report(w http.ResponseWriter, r *http.Request) {
	var in HCGReportRequest
	if !hcgBody(w, r, &in) {
		return
	}
	if in.Engine != reporting.EngineName {
		hcgWrite(w, r, nil, apierr.Invalid("INVALID_REPORT_ENGINE", "Only llm-usage-metrics is supported", nil))
		return
	}
	v, err := c.Svc.Report(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")))
	hcgWrite(w, r, HCGReportResponse(v), err)
}
