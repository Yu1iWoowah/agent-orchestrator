package controllers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

type evidenceAPI struct {
	controllers.ProjectDocumentService
	path  string
	calls int
}

func (s *evidenceAPI) ReadEvidence(context.Context, domain.ProjectID, string, string, string) (docs.File, error) {
	s.calls++
	return docs.File{Path: "transcript.md", Content: "source", Hash: "verified", Size: 6}, nil
}
func (s *evidenceAPI) OpenEvidence(context.Context, domain.ProjectID, string, string, string) (*os.File, error) {
	s.calls++
	return os.Open(s.path)
}
func TestHandoffEvidenceRoutesAreReadOnly(t *testing.T) {
	f, e := os.CreateTemp(t.TempDir(), "evidence")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.WriteString("source")
	_ = f.Close()
	svc := &evidenceAPI{path: f.Name()}
	router := chi.NewRouter()
	(&controllers.ProjectDocumentsController{Svc: svc}).Register(router)
	base := "/projects/p/handoffs/11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222"
	for _, path := range []string{"/file?path=transcript.md", "/files/transcript.md?download=1"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, base+path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if svc.calls != 2 {
		t.Fatal("route not connected")
	}
	for _, path := range []string{"/files/%252e%252e", "/files/a%2fb", "/files/../transcript.md"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, base+path, nil))
		if w.Code == 200 {
			t.Fatal("unsafe path served")
		}
	}
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, base+"/files/transcript.md", nil))
		if w.Code != 405 {
			t.Fatalf("write exposed: %d", w.Code)
		}
	}
}
