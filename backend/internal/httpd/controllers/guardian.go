package controllers

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	guardiansvc "github.com/aoagents/agent-orchestrator/backend/internal/service/guardian"
)

// GuardianQueryService exposes local stored metadata and purge.
type GuardianQueryService interface {
	Query(context.Context, domain.SessionID, guardiansvc.QueryOptions) (guardiansvc.Report, error)
	Purge(context.Context, domain.SessionID) error
}

// GuardianController handles metadata-only guardian queries.
type GuardianController struct{ Svc GuardianQueryService }

// Get reads a bounded safe metadata report.
func (c *GuardianController) Get(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/sessions/{sessionId}/guardian-observability")
		return
	}
	opts := guardiansvc.QueryOptions{}
	query := r.URL.Query()
	invalid := func() {
		envelope.WriteError(w, r, apierr.Invalid("INVALID_GUARDIAN_QUERY", "Use UTC RFC3339 dates, details=true, and a limit between 1 and 500.", nil))
	}
	for key, values := range query {
		if len(values) != 1 {
			invalid()
			return
		}
		switch key {
		case "since", "until", "details", "limit", "cursor":
		default:
			invalid()
			return
		}
	}
	for key, dest := range map[string]*time.Time{"since": &opts.Since, "until": &opts.Until} {
		raw, ok := query[key]
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, raw[0])
		if err != nil {
			invalid()
			return
		}
		_, offset := at.Zone()
		if offset != 0 {
			invalid()
			return
		}
		*dest = at.UTC()
	}

	if raw, ok := query["details"]; ok {
		if raw[0] != "true" && raw[0] != "false" {
			invalid()
			return
		}
		opts.Details = raw[0] == "true"
	}
	if raw, ok := query["limit"]; ok {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > 500 {
			invalid()
			return
		}
		opts.Limit = n
	}
	opts.Cursor = query.Get("cursor")
	if err := guardiansvc.ValidateQuery(opts); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	report, err := c.Svc.Query(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), opts)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, GuardianObservabilityResponse(report))
}

// Delete purges only the selected session’s new observation data.
func (c *GuardianController) Delete(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, http.MethodDelete, "/api/v1/sessions/{sessionId}/guardian-observability")
		return
	}
	if err := c.Svc.Purge(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId"))); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, GuardianPurgeResponse{Purged: true})
}
