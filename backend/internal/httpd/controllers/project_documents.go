package controllers

import (
	"context"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

// ProjectDocumentService defines the daemon boundary for living documents.
type ProjectDocumentService interface {
	Info(context.Context, domain.ProjectID) (docs.Info, error)
	Read(context.Context, domain.ProjectID, string) (docs.File, error)
	Open(context.Context, domain.ProjectID, string) (*os.File, error)
	Acquire(context.Context, domain.ProjectID, docs.Actor) (docs.Grant, error)
	Write(context.Context, domain.ProjectID, docs.WriteInput) (docs.File, error)
	Adopt(context.Context, domain.ProjectID, docs.AdoptInput) (domain.ProjectDocumentAdoption, error)
}

// ProjectDocumentsController serves confined reads and leased writes.
type ProjectDocumentsController struct{ Svc ProjectDocumentService }

// Register mounts project document routes.
func (c *ProjectDocumentsController) Register(r chi.Router) {
	r.Get("/projects/{id}/documents", c.info)
	r.Get("/projects/{id}/documents/file", c.read)
	r.Get("/projects/{id}/documents/files/*", c.file)
	r.Post("/projects/{id}/documents/lease", c.acquire)
	r.Put("/projects/{id}/documents/file", c.write)
	r.Post("/projects/{id}/documents/adopt", c.adopt)
}
func (c *ProjectDocumentsController) available(w http.ResponseWriter, r *http.Request, route string) bool {
	if c.Svc != nil {
		return true
	}
	apispec.NotImplemented(w, r, r.Method, "/api/v1/projects/{id}/documents"+route)
	return false
}
func (c *ProjectDocumentsController) info(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "") {
		return
	}
	info, err := c.Svc.Info(r.Context(), domain.ProjectID(chi.URLParam(r, "id")))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectDocumentsInfoResponse{Documents: info})
}
func (c *ProjectDocumentsController) read(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/file") {
		return
	}
	file, err := c.Svc.Read(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), r.URL.Query().Get("path"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectDocumentFileResponse{File: file})
}
func (c *ProjectDocumentsController) file(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/files/*") {
		return
	}
	p, err := projectDocumentFilePath(r)
	if err != nil {
		writeArtifactFileNotFound(w, r)
		return
	}
	f, err := c.Svc.Open(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), p)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; img-src 'self' data:; style-src 'unsafe-inline'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		w.Header().Set("Content-Type", t)
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(p)}))
	}
	http.ServeContent(w, r, path.Base(p), info.ModTime(), f)
}
func (c *ProjectDocumentsController) acquire(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/lease") {
		return
	}
	var in ProjectDocumentLeaseRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "INVALID_JSON", "Invalid lease request", nil)
		return
	}
	lease, err := c.Svc.Acquire(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), in.Actor)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectDocumentLeaseResponse{Lease: lease})
}
func (c *ProjectDocumentsController) write(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/file") {
		return
	}
	var in ProjectDocumentWriteRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "INVALID_JSON", "Invalid document request", nil)
		return
	}
	file, err := c.Svc.Write(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), docs.WriteInput(in))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectDocumentFileResponse{File: file})
}
func (c *ProjectDocumentsController) adopt(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/adopt") {
		return
	}
	var in ProjectDocumentAdoptRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "INVALID_JSON", "Invalid adoption request", nil)
		return
	}
	result, err := c.Svc.Adopt(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), docs.AdoptInput(in))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectDocumentAdoptResponse{Adoption: result})
}

// Decode each path segment once; an encoded separator is never another route.
func projectDocumentFilePath(r *http.Request) (string, error) {
	const marker = "/documents/files/"
	_, tail, ok := strings.Cut(r.URL.EscapedPath(), marker)
	if !ok {
		return "", apierr.Invalid("PROJECT_DOCUMENT_PATH_INVALID", "Missing document path", nil)
	}
	parts := strings.Split(tail, "/")
	for i, segment := range parts {
		decoded, err := url.PathUnescape(segment)
		if err != nil || strings.ContainsAny(decoded, "/\\") {
			return "", apierr.Invalid("PROJECT_DOCUMENT_PATH_INVALID", "Encoded path separators are not supported", nil)
		}
		parts[i] = decoded
	}
	return strings.Join(parts, "/"), nil
}
