package controllers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type documentCapability struct{}

func (documentCapability) Valid(_ domain.SessionID, token, verifier string) bool {
	return token == "actor-secret" && verifier == "actor-verifier"
}
func TestProjectDocumentsAPIKeepsWritesLeasedAndReadsSessionIndependent(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "docs", Path: t.TempDir(), RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	owner, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "docs", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{ControllerGeneration: "g1", ProviderConversationID: "n1", BrowserCapabilityVerifier: "actor-verifier"}})
	if err != nil {
		t.Fatal(err)
	}
	service := docs.New(docs.Deps{Store: db, DataDir: t.TempDir(), Capabilities: documentCapability{}})
	router := chi.NewRouter()
	(&controllers.ProjectDocumentsController{Svc: service}).Register(router)
	request := func(method, target string, payload any) *httptest.ResponseRecorder {
		t.Helper()
		var data []byte
		if payload != nil {
			var e error
			data, e = json.Marshal(payload)
			if e != nil {
				t.Fatal(e)
			}
		}
		r := httptest.NewRequest(method, target, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	actor := docs.Actor{SessionID: owner.ID, Capability: "actor-secret"}
	bad := actor
	bad.Capability = "forged"
	rejected := request(http.MethodPost, "/projects/docs/documents/lease", map[string]any{"actor": bad})
	if rejected.Code != http.StatusForbidden || !strings.Contains(rejected.Body.String(), "PROJECT_DOCUMENT_WRITER_REQUIRED") {
		t.Fatalf("forged=%d %s", rejected.Code, rejected.Body.String())
	}
	acquired := request(http.MethodPost, "/projects/docs/documents/lease", map[string]any{"actor": actor})
	if acquired.Code != http.StatusOK {
		t.Fatalf("lease=%d %s", acquired.Code, acquired.Body.String())
	}
	var grant struct {
		Lease docs.Grant `json:"lease"`
	}
	if err = json.Unmarshal(acquired.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	saved := request(http.MethodPut, "/projects/docs/documents/file", docs.WriteInput{Actor: actor, Lease: grant.Lease, Path: "handoff/notes.md", Content: "# Project living note", ExpectedHash: docs.MissingHash})
	if saved.Code != http.StatusOK {
		t.Fatalf("save=%d %s", saved.Code, saved.Body.String())
	}
	stale := request(http.MethodPut, "/projects/docs/documents/file", docs.WriteInput{Actor: actor, Lease: grant.Lease, Path: "handoff/notes.md", Content: "bad", ExpectedHash: docs.MissingHash})
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), "PROJECT_DOCUMENT_STALE") {
		t.Fatalf("stale=%d %s", stale.Code, stale.Body.String())
	}
	owner.IsTerminated = true
	if err = db.UpdateSession(ctx, owner); err != nil {
		t.Fatal(err)
	}
	read := request(http.MethodGet, "/projects/docs/documents/file?path=handoff%2Fnotes.md", nil)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), "# Project living note") {
		t.Fatalf("read=%d %s", read.Code, read.Body.String())
	}
	raw := request(http.MethodGet, "/projects/docs/documents/files/handoff/notes.md", nil)
	if raw.Code != http.StatusOK || raw.Body.String() != "# Project living note" || !strings.Contains(raw.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("raw=%d %s", raw.Code, raw.Body.String())
	}
	location, err := service.Info(ctx, "docs")
	if err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte{0, 0x99}, (3<<20)/2)
	if err = os.WriteFile(filepath.Join(location.SharedDir, "large.bin"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	preview := request(http.MethodGet, "/projects/docs/documents/file?path=large.bin", nil)
	if preview.Code != http.StatusBadRequest || !strings.Contains(preview.Body.String(), "PROJECT_DOCUMENT_TOO_LARGE") {
		t.Fatalf("large preview=%d %s", preview.Code, preview.Body.String())
	}
	download := request(http.MethodGet, "/projects/docs/documents/files/large.bin?download=1", nil)
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), large) {
		t.Fatalf("streaming download=%d bytes=%d", download.Code, download.Body.Len())
	}

	for _, target := range []string{"/projects/other/documents/file?path=handoff%2Fnotes.md", "/projects/docs/documents/file?path=..%2Foutside", "/projects/docs/documents/files/handoff%2Fnotes.md", "/projects/docs/documents/files/handoff/%256eotes.md"} {
		w := request(http.MethodGet, target, nil)
		if w.Code == http.StatusOK {
			t.Fatalf("escaped=%s %s", target, w.Body.String())
		}
	}
	info := request(http.MethodGet, "/projects/docs/documents", nil)
	if info.Code != http.StatusOK || strings.Contains(info.Body.String(), grant.Lease.Token) || strings.Contains(info.Body.String(), "actor-verifier") {
		t.Fatalf("unsafe info=%d %s", info.Code, info.Body.String())
	}
}
