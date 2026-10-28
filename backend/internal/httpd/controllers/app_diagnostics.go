package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	diag "github.com/aoagents/agent-orchestrator/backend/internal/service/appdiagnostics"
)

// AppDiagnosticsService is the local capability-checked read boundary. Grant is absent.
type AppDiagnosticsService interface {
	Status(context.Context, string) (diag.RuntimeState, error)
	Capture(context.Context, string, diag.CaptureRequest) (diag.Record, error)
	Read(context.Context, string, string) (diag.Record, error)
	RevokeID(context.Context, string, string) error
	List(context.Context, string) (diag.SnapshotList, error)
	Audits(context.Context, string) (diag.AuditLog, error)
}

// AppDiagnosticsController cannot enable diagnostics or mint credentials.
type AppDiagnosticsController struct{ Svc AppDiagnosticsService }

const appDiagnosticsCapabilityHeader = "X-AO-App-Diagnostics-Capability"

// Register mounts only desktop-only capability-gated routes.
func (c *AppDiagnosticsController) Register(r chi.Router) {
	r.Route("/desktop/diagnostics", func(r chi.Router) {
		r.Use(c.boundary)
		r.Get("/status", c.status)
		r.Get("/snapshots", c.list)
		r.Get("/audit", c.audit)
		r.Get("/windows", c.status)
		r.Post("/snapshots", c.capture)
		r.Get("/snapshots/{snapshotId}", c.read)
		r.Delete("/grants/{grantId}", c.revoke)
	})
}
func (c *AppDiagnosticsController) boundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(remote)
		browser := r.Header.Get("Origin") != ""
		for key := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "sec-fetch-") {
				browser = true
			}
		}
		if requestscope.IsLAN(r.Context()) || ip == nil || !ip.IsLoopback() || (host != "127.0.0.1" && host != "localhost" && host != "[::1]" && host != "::1") || browser {
			envelope.WriteAPIError(w, r, 403, "forbidden", "APP_DIAGNOSTICS_LOCAL_ONLY", "App diagnostics requires a native local client", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (c *AppDiagnosticsController) fail(w http.ResponseWriter, r *http.Request, err error) {
	var failure *diag.Error
	if errors.As(err, &failure) {
		envelope.WriteAPIError(w, r, failure.Status, "app_diagnostics", failure.Code, failure.Code, nil)
		return
	}
	envelope.WriteAPIError(w, r, 500, "internal", "APP_DIAGNOSTICS_STORAGE_FAILED", "App diagnostics failed", nil)
}
func (c *AppDiagnosticsController) status(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	out, err := c.Svc.Status(r.Context(), r.Header.Get(appDiagnosticsCapabilityHeader))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	envelope.WriteJSON(w, 200, out)
}
func (c *AppDiagnosticsController) capture(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	var in diag.CaptureRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil || decoder.Decode(new(any)) != io.EOF {
		envelope.WriteAPIError(w, r, 400, "invalid_request", "APP_DIAGNOSTICS_INVALID_ARGUMENT", "Invalid app snapshot request", nil)
		return
	}
	if len(in.ProjectID) > 128 || len(in.WindowID) > 128 {
		envelope.WriteAPIError(w, r, 400, "invalid_request", "APP_DIAGNOSTICS_INVALID_ARGUMENT", "Invalid app snapshot request", nil)
		return
	}
	out, err := c.Svc.Capture(r.Context(), r.Header.Get(appDiagnosticsCapabilityHeader), in)
	if err != nil {
		c.fail(w, r, err)
		return
	}
	envelope.WriteJSON(w, 201, out)
}
func (c *AppDiagnosticsController) read(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	out, err := c.Svc.Read(r.Context(), r.Header.Get(appDiagnosticsCapabilityHeader), chi.URLParam(r, "snapshotId"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	envelope.WriteJSON(w, 200, out)
}
func (c *AppDiagnosticsController) revoke(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	token := r.Header.Get(appDiagnosticsCapabilityHeader)
	if _, err := c.Svc.Status(r.Context(), token); err != nil {
		c.fail(w, r, err)
		return
	}
	if err := c.Svc.RevokeID(r.Context(), token, chi.URLParam(r, "grantId")); err != nil {
		c.fail(w, r, err)
		return
	}
	envelope.WriteJSON(w, 200, AppDiagnosticsRevokeResponse{Revoked: true})
}

func (c *AppDiagnosticsController) list(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	out, err := c.Svc.List(r.Context(), r.Header.Get(appDiagnosticsCapabilityHeader))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	envelope.WriteJSON(w, 200, out)
}
func (c *AppDiagnosticsController) audit(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r) {
		return
	}
	out, err := c.Svc.Audits(r.Context(), r.Header.Get(appDiagnosticsCapabilityHeader))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	envelope.WriteJSON(w, 200, out)
}

func (c *AppDiagnosticsController) available(w http.ResponseWriter, r *http.Request) bool {
	if c.Svc != nil {
		return true
	}
	envelope.WriteAPIError(w, r, 503, "unavailable", "APP_DIAGNOSTICS_UNSUPPORTED", "App diagnostics is unavailable", nil)
	return false
}
