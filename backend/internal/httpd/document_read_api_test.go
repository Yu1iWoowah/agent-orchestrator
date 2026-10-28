package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/documentread"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestDocumentReadLocalPathsUseEncodedManagedProjectRoots(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpenAt(t, t.TempDir())
	data := t.TempDir()
	shared := projectdocuments.New(projectdocuments.Deps{Store: db, DataDir: data})
	router := NewRouterWithControl(config.Config{DataDir: data}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		APIDeps{DocumentRead: documentread.New(documentread.Deps{Store: db, Shared: shared, DataDir: data})}, ControlDeps{})
	for _, id := range []string{"next.js", "MyApp", "con"} {
		t.Run(id, func(t *testing.T) {
			if e := db.UpsertProject(ctx, domain.ProjectRecord{ID: id, Path: t.TempDir(), RegisteredAt: time.Now()}); e != nil {
				t.Fatal(e)
			}
			// ReadRoot uses production projectKey; do not construct its encoded directory in the fixture.
			root, e := shared.ReadRoot(ctx, domain.ProjectID(id))
			if e != nil {
				t.Fatal(e)
			}
			if e = os.MkdirAll(root, 0o700); e != nil {
				t.Fatal(e)
			}
			full := filepath.Join(root, "guide.md")
			if e = os.WriteFile(full, []byte("shared guide"), 0o600); e != nil {
				t.Fatal(e)
			}
			urlPath := filepath.ToSlash(full)
			if !strings.HasPrefix(urlPath, "/") {
				urlPath = "/" + urlPath
			}
			for _, target := range []any{
				documentread.SharedTarget{Kind: "shared", ProjectID: id, Path: "guide.md"},
				documentread.LocalPathTarget{Kind: "local-path", Value: full},
				documentread.LocalPathTarget{Kind: "local-path", Value: (&url.URL{Scheme: "file", Path: urlPath}).String()},
			} {
				body, e := json.Marshal(target)
				if e != nil {
					t.Fatal(e)
				}
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/local-documents/resolve", bytes.NewReader(body)))
				var response struct {
					Document struct {
						LocalPath    string `json:"localPath"`
						CanonicalURI string `json:"canonicalUri"`
					} `json:"document"`
				}
				if e = json.Unmarshal(w.Body.Bytes(), &response); e != nil {
					t.Fatal(e)
				}
				if w.Code != http.StatusOK || response.Document.LocalPath != full || response.Document.CanonicalURI != "ao://projects/"+id+"/shared/guide.md" {
					t.Errorf("target %s: HTTP %d %s", body, w.Code, w.Body.String())
				}
			}
			rootURL := (&url.URL{Scheme: "file", Path: strings.TrimSuffix(urlPath, "/guide.md")}).String()
			for _, value := range []string{root + "//guide.md", rootURL + "//guide.md", rootURL + "%2Fguide.md", rootURL + "%2fguide.md", rootURL + "/%252Fguide.md"} {
				body, e := json.Marshal(documentread.LocalPathTarget{Kind: "local-path", Value: value})
				if e != nil {
					t.Fatal(e)
				}
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/local-documents/resolve", bytes.NewReader(body)))
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "DOCUMENT_PATH_INVALID") {
					t.Errorf("ambiguous path %q: HTTP %d %s", value, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestDocumentReadAPIUsesRegisteredReadRootsWithoutMutatingOwnership(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpenAt(t, t.TempDir())
	data := t.TempDir()
	if e := db.UpsertProject(ctx, domain.ProjectRecord{ID: "docs", Path: t.TempDir(), RegisteredAt: time.Now()}); e != nil {
		t.Fatal(e)
	}
	rec, e := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "docs", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, IsTerminated: true, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(data, "artifacts", string(rec.ID))
	if e = os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "report.md"), []byte("private document"), 0600); e != nil {
		t.Fatal(e)
	}
	shared := projectdocuments.New(projectdocuments.Deps{Store: db, DataDir: data})
	before, e := db.GetProjectDocumentState(ctx, "docs")
	if e != nil {
		t.Fatal(e)
	}
	router := NewRouterWithControl(config.Config{DataDir: data}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, APIDeps{DocumentRead: documentread.New(documentread.Deps{Store: db, Shared: shared, DataDir: data})}, ControlDeps{})
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, r)
		return w
	}
	body := fmtTarget(t, string(rec.ID))
	r := request(http.MethodPost, "/api/v1/local-documents/resolve", body)
	if r.Code != 200 || !strings.Contains(r.Body.String(), filepath.ToSlash(dir)) || strings.Contains(r.Body.String(), "private document") || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("resolve %d %s", r.Code, r.Body.String())
	}
	for _, bad := range []string{body + ` {}`, `{"kind":"shared","projectId":"docs","sessionId":"forged","path":"report.md"}`, `{"kind":"artifact","projectId":"other","sessionId":"` + string(rec.ID) + `","path":"report.md"}`, `{"kind":"local-path","value":"` + filepath.ToSlash(filepath.Join(data, "ao.db")) + `"}`} {
		w := request(http.MethodPost, "/api/v1/local-documents/resolve", bad)
		if w.Code != 400 && w.Code != 404 {
			t.Errorf("bad target: %d %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"requestId":`) {
			t.Error("lost error envelope")
		}
	}
	for _, query := range []string{"", "?source=orchestrator&archive=archive&limit=1"} {
		w := request(http.MethodGet, "/api/v1/projects/docs/document-catalog"+query, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "report.md") || strings.Contains(w.Body.String(), data) || strings.Contains(w.Body.String(), "localPath") {
			t.Errorf("catalog %d %s", w.Code, w.Body.String())
		}
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?limit=bad", "?source=all&source=shared", "?archive=bad"} {
		if w := request(http.MethodGet, "/api/v1/projects/docs/document-catalog"+query, ""); w.Code != 400 {
			t.Errorf("query %s: %d", query, w.Code)
		}
	}
	after, e := db.GetProjectDocumentState(ctx, "docs")
	if e != nil {
		t.Fatal(e)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatalf("read mutated document ownership: %s -> %s", a, b)
	}
	stored, ok, e := db.GetSession(ctx, rec.ID)
	if e != nil || !ok || !stored.IsTerminated || stored.Metadata.ArtifactDir != "" {
		t.Fatalf("read mutated session: %+v %v", stored, e)
	}
	if _, e = os.Stat(filepath.Join(data, "projects")); !os.IsNotExist(e) {
		t.Fatalf("read created shared tree: %v", e)
	}
}
func fmtTarget(t *testing.T, id string) string {
	t.Helper()
	b, e := json.Marshal(map[string]string{"kind": "artifact", "projectId": "docs", "sessionId": id, "path": "report.md"})
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}

func TestDocumentResolverBlockedOnAuthenticatedLANAndTunnel(t *testing.T) {
	router := NewRouterWithControl(config.Config{AllowedOrigins: []string{"app://renderer"}}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, APIDeps{}, ControlDeps{})
	m := NewMobileLAN(router, "test-host", 0, nil, nil)
	m.SetPasswordHash(mobilebridge.HashPassword("secret-password"))
	for _, path := range []string{"/api/v1/local-documents/resolve", "/api/v1/local%2Ddocuments/resolve", "/api/v1/local-documents%2Fresolve", "/api/v1/./local-documents/resolve"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		r.Host = "127.0.0.1"
		r.Header.Set("Origin", "app://renderer")
		r.Header.Set("Authorization", "Bearer secret-password")
		w := httptest.NewRecorder()
		m.handler.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Errorf("LAN/tunnel %s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
