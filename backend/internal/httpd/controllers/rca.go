package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// RCAService exposes numeric queries and local RCA controls.
type RCAService interface {
	Get(context.Context, domain.SessionID, domain.RCAQuery) (domain.RCAComposition, error)
	SetOverride(context.Context, domain.SessionID, *bool) (domain.RCAPolicyView, error)
	EmergencyStop(context.Context, bool) error
	Purge(context.Context, domain.SessionID) error
}

// RCAInlineService is native raw admission, kept separate from numeric callers.
type RCAInlineService interface {
	AdmitClaude(context.Context, string) (ports.RCAInlineAdmission, string)
	RejectClaude(string, string)
}

// RCAController serves the fixed RCA HTTP surface.
type RCAController struct{ Svc RCAService }

// Register mounts composition queries and RCA controls.
func (c RCAController) Register(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if c.Svc == nil {
					envelope.WriteAPIError(w, req, 501, "not_implemented", "RCA_UNAVAILABLE", "unavailable", nil)
					return
				}
				next.ServeHTTP(w, req)
			})
		})
		r.Post("/rca/otlp/v1/logs", c.ingest)
		r.Get("/usage/sessions/{sessionId}/composition", c.get)
		r.Delete("/usage/sessions/{sessionId}/composition", c.purge)
		r.Put("/sessions/{sessionId}/rca/override", c.set)
		r.Delete("/sessions/{sessionId}/rca/override", c.clear)
		r.Put("/rca/emergency-stop", c.stop)
	})
}

func rcaWrite(w http.ResponseWriter, r *http.Request, value any, err error) {
	if err != nil {
		status, kind, code, message := 500, "internal_error", "RCA_UNAVAILABLE", "unavailable"
		var typed *apierr.Error
		if errors.As(err, &typed) {
			switch typed.Code {
			case "SESSION_NOT_FOUND":
				status, kind, code, message = 404, "not_found", typed.Code, "Unknown session"
			case "RCA_SCOPE_NOT_ALLOWED":
				status, kind, code, message = 403, "forbidden", typed.Code, "scope_not_allowed"
			case "INVALID_USAGE_PAGE":
				status, kind, code, message = 400, "bad_request", typed.Code, "Invalid composition paging"
			}
		}
		envelope.WriteAPIError(w, r, status, kind, code, message, nil)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, value)
}

func (c RCAController) get(w http.ResponseWriter, r *http.Request) {
	q := domain.RCAQuery{Limit: 200}
	values := r.URL.Query()
	for key, items := range values {
		if len(items) != 1 {
			rcaWrite(w, r, nil, apierr.Invalid("INVALID_USAGE_PAGE", "Repeated parameter", nil))
			return
		}
		switch key {
		case "afterId", "throughId", "limit":
			n, err := strconv.ParseInt(items[0], 10, 64)
			if err != nil {
				rcaWrite(w, r, nil, apierr.Invalid("INVALID_USAGE_PAGE", "Invalid paging", nil))
				return
			}
			switch key {
			case "afterId":
				q.AfterID = n
			case "throughId":
				q.ThroughID = &n
			default:
				q.Limit = n
			}
		case "analyze":
			if items[0] != "true" && items[0] != "false" {
				rcaWrite(w, r, nil, apierr.Invalid("INVALID_USAGE_PAGE", "Invalid analyze flag", nil))
				return
			}
			q.Analyze = items[0] == "true"
		default:
			rcaWrite(w, r, nil, apierr.Invalid("INVALID_USAGE_PAGE", "Unknown parameter", nil))
			return
		}
	}
	if q.AfterID < 0 || q.Limit < 1 || q.Limit > 1000 || (q.ThroughID == nil && q.AfterID != 0) || (q.ThroughID != nil && (*q.ThroughID < q.AfterID || *q.ThroughID < 0)) {
		rcaWrite(w, r, nil, apierr.Invalid("INVALID_USAGE_PAGE", "Invalid paging", nil))
		return
	}
	value, err := c.Svc.Get(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), q)
	rcaWrite(w, r, RCACompositionResponse(value), err)
}

func rcaEnabled(w http.ResponseWriter, r *http.Request) (bool, bool) {
	var body RCAEnabledRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF || body.Enabled == nil {
		envelope.WriteAPIError(w, r, 400, "bad_request", "INVALID_RCA_REQUEST", "enabled must be an explicit boolean", nil)
		return false, false
	}
	return *body.Enabled, true
}
func (c RCAController) set(w http.ResponseWriter, r *http.Request) {
	enabled, ok := rcaEnabled(w, r)
	if !ok {
		return
	}
	v, err := c.Svc.SetOverride(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), &enabled)
	rcaWrite(w, r, RCAPolicyResponse(v), err)
}
func (c RCAController) clear(w http.ResponseWriter, r *http.Request) {
	v, err := c.Svc.SetOverride(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), nil)
	rcaWrite(w, r, RCAPolicyResponse(v), err)
}
func (c RCAController) stop(w http.ResponseWriter, r *http.Request) {
	enabled, ok := rcaEnabled(w, r)
	if !ok {
		return
	}
	err := c.Svc.EmergencyStop(r.Context(), enabled)
	rcaWrite(w, r, RCAStopResponse{Enabled: enabled}, err)
}
func (c RCAController) purge(w http.ResponseWriter, r *http.Request) {
	err := c.Svc.Purge(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")))
	rcaWrite(w, r, RCAPurgeResponse{Purged: true}, err)
}

func (c RCAController) ingest(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(RCAInlineService)
	interrupt := requestscope.RCARawInterrupt(r.Context())
	if !ok || interrupt == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	markers := r.Header.Values("X-AO-RCA-Capture")
	if len(markers) != 1 {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	admission, why := svc.AdmitClaude(r.Context(), markers[0])
	if why != "" {
		status := http.StatusServiceUnavailable
		if why == "source_stopped" {
			status = http.StatusForbidden
		}
		w.WriteHeader(status)
		return
	}
	if !requestscope.OnRCARawFinished(r.Context(), admission.Release) {
		admission.Release()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	stop := context.AfterFunc(admission.Context(), interrupt)
	defer stop()
	why = admission.Read(r.Body, r.ContentLength)
	if why != "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}
