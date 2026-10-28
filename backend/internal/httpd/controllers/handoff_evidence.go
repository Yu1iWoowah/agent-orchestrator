package controllers

import (
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

func (c *ProjectDocumentsController) evidenceRead(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/handoffs") {
		return
	}
	file, err := c.Svc.ReadEvidence(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), chi.URLParam(r, "operationId"), chi.URLParam(r, "attemptId"), r.URL.Query().Get("path"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectDocumentFileResponse{File: file})
}
func (c *ProjectDocumentsController) evidenceDownload(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/handoffs") {
		return
	}
	p := chi.URLParam(r, "path")
	decoded, err := url.PathUnescape(p)
	if err != nil || decoded != p || strings.ContainsAny(p, "/\\%") || p != "handoff.md" && p != "transcript.md" && p != "manifest.json" {
		writeArtifactFileNotFound(w, r)
		return
	}
	// Check each raw segment before chi's decoding; encoded slashes, aliases and
	// double-encoded traversal never reach the confined service boundary.
	for _, part := range strings.Split(r.URL.EscapedPath(), "/") {
		d, e := url.PathUnescape(part)
		if e != nil || strings.ContainsAny(d, "/\\%") || d == "." || d == ".." {
			writeArtifactFileNotFound(w, r)
			return
		}
	}
	f, err := c.Svc.OpenEvidence(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), chi.URLParam(r, "operationId"), chi.URLParam(r, "attemptId"), p)
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
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": p}))
	}
	http.ServeContent(w, r, p, info.ModTime(), f)
}
