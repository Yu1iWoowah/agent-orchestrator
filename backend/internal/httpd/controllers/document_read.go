package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/attachmentstore"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/documentread"
)

// DocumentReadService is the side-effect-free document discovery boundary.
type DocumentReadService interface {
	Resolve(context.Context, documentread.Input) (documentread.Resolved, error)
	Read(context.Context, documentread.Input, int64, string) (documentread.Content, error)
	Catalog(context.Context, string, documentread.CatalogQuery) (documentread.Catalog, error)
}

// DocumentReadController serves managed metadata and local path resolution.
type DocumentReadController struct{ Svc DocumentReadService }

// Register mounts the document read routes.
func (c *DocumentReadController) Register(r chi.Router) {
	r.Post("/local-documents/resolve", c.resolve)
	r.Post("/local-documents/content", c.content)
	r.Get("/projects/{id}/document-catalog", c.catalog)
}
func (c *DocumentReadController) availableLocal(w http.ResponseWriter, r *http.Request, route string) bool {
	w.Header().Set("Cache-Control", "no-store")
	if requestscope.IsLAN(r.Context()) {
		envelope.WriteAPIError(w, r, 404, "not_found", "ROUTE_NOT_FOUND", "Document reads are unavailable on this connection", nil)
		return false
	}
	if c.Svc == nil {
		apispec.NotImplemented(w, r, http.MethodPost, route)
		return false
	}
	return true
}
func documentInput(w http.ResponseWriter, r *http.Request) (documentread.Input, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var input documentread.Input
	d := json.NewDecoder(r.Body)
	if e := d.Decode(&input); e != nil {
		envelope.WriteAPIError(w, r, 400, "validation", "DOCUMENT_REQUEST_INVALID", "A valid document target is required", nil)
		return input, false
	}
	if e := d.Decode(new(any)); e != io.EOF {
		envelope.WriteAPIError(w, r, 400, "validation", "DOCUMENT_REQUEST_INVALID", "Exactly one document target is required", nil)
		return input, false
	}
	return input, true
}
func (c *DocumentReadController) resolve(w http.ResponseWriter, r *http.Request) {
	if !c.availableLocal(w, r, "/api/v1/local-documents/resolve") {
		return
	}
	input, ok := documentInput(w, r)
	if !ok {
		return
	}
	doc, e := c.Svc.Resolve(r.Context(), input)
	if e != nil {
		envelope.WriteError(w, r, e)
		return
	}
	envelope.WriteJSON(w, 200, ResolveDocumentResponse{Document: doc})
}
func (c *DocumentReadController) content(w http.ResponseWriter, r *http.Request) {
	if !c.availableLocal(w, r, "/api/v1/local-documents/content") {
		return
	}
	values := r.URL.Query()
	limit := int64(2 << 20)
	for _, v := range values {
		if len(v) != 1 {
			envelope.WriteAPIError(w, r, 400, "validation", "DOCUMENT_QUERY_INVALID", "Repeated parameters are not supported", nil)
			return
		}
	}
	if values.Has("maxBytes") {
		value, e := strconv.ParseInt(values.Get("maxBytes"), 10, 64)
		if e != nil || value < 1 || value > attachmentstore.MaxFileBytes {
			envelope.WriteAPIError(w, r, 400, "validation", "DOCUMENT_QUERY_INVALID", "A valid byte limit is required", nil)
			return
		}
		limit = value
	}
	if len(values.Get("fingerprint")) > 256 {
		envelope.WriteAPIError(w, r, 400, "validation", "DOCUMENT_QUERY_INVALID", "Invalid document fingerprint", nil)
		return
	}
	input, ok := documentInput(w, r)
	if !ok {
		return
	}
	out, e := c.Svc.Read(r.Context(), input, limit, values.Get("fingerprint"))
	if e != nil {
		envelope.WriteError(w, r, e)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("ETag", strconv.Quote(out.Document.Fingerprint))
	w.Header().Set("Content-Length", strconv.Itoa(len(out.Bytes)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Bytes)
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
