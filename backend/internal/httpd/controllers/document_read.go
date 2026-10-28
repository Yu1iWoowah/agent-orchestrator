package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/documentread"
	"github.com/go-chi/chi/v5"
)

// DocumentReadService is the side-effect-free document discovery boundary.
type DocumentReadService interface {
	Resolve(context.Context, documentread.Input) (documentread.Resolved, error)
	Catalog(context.Context, string, documentread.CatalogQuery) (documentread.Catalog, error)
}

// DocumentReadController serves managed metadata and local path resolution.
type DocumentReadController struct{ Svc DocumentReadService }

// Register mounts the document read routes.
func (c *DocumentReadController) Register(r chi.Router) {
	r.Post("/local-documents/resolve", c.resolve)
	r.Get("/projects/{id}/document-catalog", c.catalog)
}
func (c *DocumentReadController) resolve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if requestscope.IsLAN(r.Context()) {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "ROUTE_NOT_FOUND", "Document path resolution is unavailable on this connection", nil)
		return
	}
	if c.Svc == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/local-documents/resolve")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var input documentread.Input
	d := json.NewDecoder(r.Body)
	if err := d.Decode(&input); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "DOCUMENT_REQUEST_INVALID", "A valid document target is required", nil)
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "DOCUMENT_REQUEST_INVALID", "Exactly one document target is required", nil)
		return
	}
	doc, err := c.Svc.Resolve(r.Context(), input)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ResolveDocumentResponse{Document: doc})
}
func (c *DocumentReadController) catalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if c.Svc == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/projects/{id}/document-catalog")
		return
	}
	values := r.URL.Query()
	limit := 0
	if values.Has("limit") {
		var err error
		limit, err = strconv.Atoi(values.Get("limit"))
		if err != nil || limit < 1 || limit > 200 {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "DOCUMENT_QUERY_INVALID", "limit must be between 1 and 200", nil)
			return
		}
	}
	for _, v := range values {
		if len(v) != 1 {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "DOCUMENT_QUERY_INVALID", "Repeated query parameters are not supported", nil)
			return
		}
	}
	result, err := c.Svc.Catalog(r.Context(), chi.URLParam(r, "id"), documentread.CatalogQuery{Source: values.Get("source"), Archive: values.Get("archive"), Cursor: values.Get("cursor"), Limit: limit})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, DocumentCatalogResponse{Catalog: result})
}
