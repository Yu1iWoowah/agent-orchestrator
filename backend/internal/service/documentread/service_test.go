package documentread

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type readStore struct {
	projects    []domain.ProjectRecord
	sessions    []domain.SessionRecord
	sessionsErr error
}

func (f *readStore) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	for _, p := range f.projects {
		if p.ID == id {
			return p, true, nil
		}
	}
	return domain.ProjectRecord{}, false, nil
}
func (f *readStore) ListProjects(context.Context) ([]domain.ProjectRecord, error) {
	return f.projects, nil
}
func (f *readStore) ListSessions(_ context.Context, id domain.ProjectID) ([]domain.SessionRecord, error) {
	if f.sessionsErr != nil {
		return nil, f.sessionsErr
	}
	var r []domain.SessionRecord
	for _, s := range f.sessions {
		if s.ProjectID == id {
			r = append(r, s)
		}
	}
	return r, nil
}

type sharedRoots map[domain.ProjectID]string

func (f sharedRoots) ReadRoot(_ context.Context, id domain.ProjectID) (string, error) {
	return f[id], nil
}

type fixture struct {
	s      *Service
	data   string
	store  *readStore
	shared string
}

func setup(t *testing.T) fixture {
	t.Helper()
	data := t.TempDir()
	shared := filepath.Join(data, "projects", "p", "orchestrator", "shared")
	store := &readStore{projects: []domain.ProjectRecord{{ID: "p"}, {ID: "other"}}, sessions: []domain.SessionRecord{
		{ID: "p-1", ProjectID: "p", Kind: domain.KindWorker},
		{ID: "p-2", ProjectID: "p", Kind: domain.KindOrchestrator, IsTerminated: true},
		{ID: "other-1", ProjectID: "other", Kind: domain.KindOrchestrator},
	}}
	f := fixture{data: data, shared: shared, store: store}
	f.s = New(Deps{Store: store, Shared: sharedRoots{"p": shared, "other": filepath.Join(data, "projects", "other", "orchestrator", "shared")}, DataDir: data})
	f.write(t, "artifacts/p-1/notes hello.md", "worker")
	f.write(t, "artifacts/p-2/report.md", "orchestrator")
	f.write(t, "artifacts/other-1/report.md", "other")
	f.write(t, "projects/p/orchestrator/shared/guide.md", "shared")
	return f
}
func (f fixture) write(t *testing.T, p, content string) {
	t.Helper()
	full := filepath.Join(f.data, filepath.FromSlash(p))
	if e := os.MkdirAll(filepath.Dir(full), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(full, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
}
func input(t *testing.T, v string) Input {
	t.Helper()
	var i Input
	if e := json.Unmarshal([]byte(v), &i); e != nil {
		t.Fatal(e)
	}
	return i
}
func TestResolveRegisteredSourcesAndLocalPaths(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, tc := range []struct{ request, path, uri string }{
		{`{"kind":"artifact","projectId":"p","sessionId":"p-1","path":"notes hello.md"}`, "artifacts/p-1/notes hello.md", "ao://sessions/p/p-1/artifacts/notes%20hello.md"},
		{`{"kind":"artifact","projectId":"p","sessionId":"p-2","path":"report.md"}`, "artifacts/p-2/report.md", "ao://sessions/p/p-2/artifacts/report.md"},
		{`{"kind":"artifact","projectId":"other","sessionId":"other-1","path":"report.md"}`, "artifacts/other-1/report.md", "ao://sessions/other/other-1/artifacts/report.md"},
		{`{"kind":"shared","projectId":"p","path":"guide.md","fragment":"intro"}`, "projects/p/orchestrator/shared/guide.md", "ao://projects/p/shared/guide.md#intro"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r, e := f.s.Resolve(ctx, input(t, tc.request))
			if e != nil {
				t.Fatal(e)
			}
			want := filepath.Join(f.data, filepath.FromSlash(tc.path))
			if r.LocalPath != want || r.CanonicalURI != tc.uri || r.Size == 0 || r.Fingerprint == "" {
				t.Fatalf("resolved=%+v want path=%s uri=%s", r, want, tc.uri)
			}
			urlPath := filepath.ToSlash(want)
			if !strings.HasPrefix(urlPath, "/") {
				urlPath = "/" + urlPath
			}
			for _, value := range []string{want, (&url.URL{Scheme: "file", Path: urlPath}).String(), (&url.URL{Scheme: "file", Host: "localhost", Path: urlPath}).String(), (&url.URL{Scheme: "file", Host: "LOCALHOST", Path: urlPath}).String()} {
				b, _ := json.Marshal(map[string]string{"kind": "local-path", "value": value})
				rr, e := f.s.Resolve(ctx, input(t, string(b)))
				if e != nil || rr.LocalPath != want {
					t.Fatalf("local %s: %+v %v", value, rr, e)
				}
			}
		})
	}
}
func TestResolveRejectsUnmanagedPathsAndMalformedSources(t *testing.T) {
	f := setup(t)
	f.write(t, "ao.db", "private")
	f.write(t, "keys/token", "private")
	f.write(t, "electron/Default/Preferences", "private")
	f.write(t, "artifacts/p-1-other/notes.md", "private")
	f.write(t, "artifacts/unregistered/report.md", "private")
	f.write(t, "artifacts/p-1/.versions/old.md", "private")
	for _, p := range []string{"artifacts/p-1/%2e%2e/notes.md", "artifacts/p-1/%252e%252e/notes.md", "artifacts/p-1/%2fnotes.md"} {
		f.write(t, p, "encoded unsafe name")
	}
	for _, p := range []string{"ao.db", "keys/token", "electron/Default/Preferences", "artifacts/p-1-other/notes.md", "artifacts/unregistered/report.md", "artifacts/p-1/.versions/old.md", "artifacts/p-1/%2e%2e/notes.md", "artifacts/p-1/%252e%252e/notes.md", "artifacts/p-1/%2fnotes.md"} {
		full := filepath.Join(f.data, p)
		urlPath := filepath.ToSlash(full)
		if !strings.HasPrefix(urlPath, "/") {
			urlPath = "/" + urlPath
		}
		for _, value := range []string{full, (&url.URL{Scheme: "file", Path: urlPath}).String()} {
			b, _ := json.Marshal(map[string]string{"kind": "local-path", "value": value})
			if _, e := f.s.Resolve(context.Background(), input(t, string(b))); e == nil {
				t.Errorf("accepted %s", value)
			}
		}
	}
	for _, s := range []string{
		`{"kind":"artifact","projectId":"other","sessionId":"p-1","path":"notes hello.md"}`,
		`{"kind":"artifact","projectId":"missing","sessionId":"p-1","path":"notes hello.md"}`,
		`{"kind":"artifact","projectId":"p","sessionId":"p-1","path":"../ao.db"}`,
		`{"kind":"artifact","projectId":"p","sessionId":"p-1","path":"%2e%2e/ao.db"}`,
		`{"kind":"shared","projectId":"p","path":"%252e%252e/ao.db"}`,
		`{"kind":"shared","projectId":"p","path":"/ao.db"}`,
		`{"kind":"shared","projectId":"p","path":".ao-private"}`,
		`{"kind":"shared","projectId":"p","path":"missing.md"}`,
		`{"kind":"local-path","value":"file://server/etc/passwd"}`,
		`{"kind":"local-path","value":"file:///etc/passwd?query=1"}`,
		`{"kind":"local-path","value":"/etc/passwd"}`,
	} {
		if _, e := f.s.Resolve(context.Background(), input(t, s)); e == nil {
			t.Errorf("accepted %s", s)
		}
	}
	for _, s := range []string{`{"kind":"shared","projectId":"p","sessionId":"p-1","path":"guide.md"}`, `{"kind":"local-path","value":"/x","path":"guide.md"}`, `{"kind":"unknown"}`} {
		var i Input
		if e := json.Unmarshal([]byte(s), &i); e == nil {
			t.Errorf("accepted malformed union %s", s)
		}
	}
}
func TestResolveRejectsSymlinksAndPersistedRootRedirection(t *testing.T) {
	f := setup(t)
	for _, pair := range [][2]string{{"artifacts/p-1/link.md", "projects/p/orchestrator/shared/guide.md"}, {"artifacts/p-1/dir", "projects/p/orchestrator/shared"}} {
		if e := os.Symlink(filepath.Join(f.data, pair[1]), filepath.Join(f.data, pair[0])); e != nil {
			t.Skip(e)
		}
	}
	for _, p := range []string{"link.md", "dir/guide.md", "."} {
		b, _ := json.Marshal(map[string]string{"kind": "artifact", "projectId": "p", "sessionId": "p-1", "path": p})
		if _, e := f.s.Resolve(context.Background(), input(t, string(b))); e == nil {
			t.Errorf("accepted %s", p)
		}
	}
	f.store.sessions[0].Metadata.ArtifactDir = f.shared
	if _, e := f.s.Resolve(context.Background(), input(t, `{"kind":"artifact","projectId":"p","sessionId":"p-1","path":"notes hello.md"}`)); e == nil {
		t.Fatal("trusted persisted artifact_dir redirection")
	}
}
func TestMetadataRejectsRootReplacedAfterOpening(t *testing.T) {
	f := setup(t)
	rootPath := filepath.Join(f.data, "artifacts", "p-1")
	root, e := f.s.openRoot(rootPath)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = root.Close() }()
	if e = os.Rename(rootPath, rootPath+"-old"); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(f.shared, rootPath); e != nil {
		t.Skip(e)
	}
	if _, e = f.s.metadata(context.Background(), root, rootPath, "notes hello.md"); e == nil {
		t.Fatal("returned path after managed root was replaced")
	}
	if e := os.Remove(rootPath); e != nil {
		t.Fatal(e)
	}
	f.write(t, "artifacts/p-1/notes hello.md", "replacement in a different regular tree")
	if _, e = f.s.metadata(context.Background(), root, rootPath, "notes hello.md"); e == nil {
		t.Fatal("returned path after managed root was replaced by another regular directory")
	}
}
func TestCatalogIncludesAllSourcesAndArchiveWithoutLocalPaths(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r, e := f.s.Catalog(ctx, "p", CatalogQuery{})
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Items) != 3 || r.Incomplete || len(r.Errors) != 0 {
		t.Fatalf("catalog %+v", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), f.data) || strings.Contains(string(b), "localPath") {
		t.Fatalf("catalog leaked path: %s", b)
	}
	for _, tc := range []struct {
		source, archive string
		count           int
	}{{"worker", "all", 1}, {"orchestrator", "archive", 1}, {"shared", "current", 1}, {"all", "current", 2}, {"all", "archive", 1}} {
		r, e := f.s.Catalog(ctx, "p", CatalogQuery{Source: tc.source, Archive: tc.archive})
		if e != nil || len(r.Items) != tc.count {
			t.Errorf("%+v => %+v %v", tc, r, e)
		}
	}
}
func TestCatalogPagesBeyondLegacyCapsAndInvalidatesChangedCursor(t *testing.T) {
	f := setup(t)
	for n := 0; n < 4100; n++ {
		f.write(t, fmt.Sprintf("projects/p/orchestrator/shared/files/%04d.txt", n), "x")
	}
	ctx := context.Background()
	q := CatalogQuery{Source: "shared", Limit: 200}
	seen := map[string]bool{}
	first, e := f.s.Catalog(ctx, "p", q)
	if e != nil {
		t.Fatal(e)
	}
	cursor := first.NextCursor
	for {
		r, e := f.s.Catalog(ctx, "p", q)
		if e != nil {
			t.Fatal(e)
		}
		for _, i := range r.Items {
			if seen[i.Path] {
				t.Fatal("duplicate", i.Path)
			}
			seen[i.Path] = true
		}
		if r.NextCursor == "" {
			break
		}
		q.Cursor = r.NextCursor
	}
	if len(seen) != 4101 {
		t.Fatalf("lost files beyond legacy cap: %d", len(seen))
	}
	f.write(t, "projects/p/orchestrator/shared/new.md", "changed")
	if _, e := f.s.Catalog(ctx, "p", CatalogQuery{Source: "shared", Limit: 200, Cursor: cursor}); e == nil {
		t.Fatal("accepted changed inventory cursor")
	}
	if _, e := f.s.Catalog(ctx, "other", CatalogQuery{Source: "shared", Limit: 200, Cursor: cursor}); e == nil {
		t.Fatal("accepted cross-project cursor")
	}
	if _, e := f.s.Catalog(ctx, "p", CatalogQuery{Source: "worker", Limit: 200, Cursor: cursor}); e == nil {
		t.Fatal("accepted cross-filter cursor")
	}
}
func TestCatalogPreservesHealthySourcesWhenOneRootFails(t *testing.T) {
	f := setup(t)
	root := filepath.Join(f.data, "artifacts", "p-1")
	if e := os.Rename(root, root+"-old"); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(f.shared, root); e != nil {
		t.Skip(e)
	}
	r, e := f.s.Catalog(context.Background(), "p", CatalogQuery{})
	if e != nil {
		t.Fatal(e)
	}
	if !r.Incomplete || len(r.Errors) != 1 || len(r.Items) != 2 {
		t.Fatalf("partial %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = f.s.Catalog(ctx, "p", CatalogQuery{}); e == nil {
		t.Fatal("ignored cancellation")
	}
	for _, q := range []CatalogQuery{{Source: "wrong"}, {Archive: "wrong"}, {Limit: 201}, {Cursor: "not-a-cursor"}} {
		if _, e := f.s.Catalog(context.Background(), "p", q); e == nil {
			t.Errorf("accepted %+v", q)
		}
	}
}
func TestCatalogDetectsMetadataChangeWithoutAddingFiles(t *testing.T) {
	f := setup(t)
	q := CatalogQuery{Limit: 1}
	r, e := f.s.Catalog(context.Background(), "p", q)
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(f.data, "artifacts", "p-1", "notes hello.md")
	tm := time.Now().Add(time.Hour)
	if e = os.Chtimes(p, tm, tm); e != nil {
		t.Fatal(e)
	}
	q.Cursor = r.NextCursor
	if _, e = f.s.Catalog(context.Background(), "p", q); e == nil {
		t.Fatal("accepted metadata change")
	}
}

func TestResolveDoesNotNormalizeTraversalDuringHomeExpansion(t *testing.T) {
	f := setup(t)
	t.Setenv("HOME", f.data)
	t.Setenv("USERPROFILE", f.data)
	if _, err := f.s.Resolve(context.Background(), input(t, `{"kind":"local-path","value":"~/artifacts/p-1/../p-1/notes hello.md"}`)); err == nil {
		t.Fatal("home expansion silently normalized traversal")
	}
}

func TestCatalogUsesExistingArtifactIgnoredEntryRules(t *testing.T) {
	f := setup(t)
	for _, p := range []string{"node_modules/cache.js", "__pycache__/cache.pyc", "Thumbs.db", "._resource"} {
		f.write(t, "artifacts/p-1/"+p, "tool data")
	}
	result, err := f.s.Catalog(context.Background(), "p", CatalogQuery{Source: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Path != "notes hello.md" {
		t.Fatalf("tool internals entered artifact catalog: %+v", result.Items)
	}
}

func TestCatalogReportsMissingManagedRootInsteadOfCompleteEmptySource(t *testing.T) {
	f := setup(t)
	if e := os.RemoveAll(filepath.Join(f.data, "artifacts", "p-1")); e != nil {
		t.Fatal(e)
	}
	result, e := f.s.Catalog(context.Background(), "p", CatalogQuery{})
	if e != nil {
		t.Fatal(e)
	}
	if !result.Incomplete || len(result.Errors) != 1 || result.Errors[0].Code != "DOCUMENT_SOURCE_MISSING" || result.Errors[0].SessionID != "p-1" || len(result.Items) != 2 {
		t.Fatalf("missing source was not distinguished from complete empty inventory: %+v", result)
	}
	if e := os.Mkdir(filepath.Join(f.data, "artifacts", "p-1"), 0o755); e != nil {
		t.Fatal(e)
	}
	result, e = f.s.Catalog(context.Background(), "p", CatalogQuery{})
	if e != nil || result.Incomplete || len(result.Errors) != 0 || len(result.Items) != 2 {
		t.Fatalf("existing empty root was reported missing: %+v %v", result, e)
	}
}

func TestLiteralPercentDocumentsResolveAndRemainInCompleteCatalog(t *testing.T) {
	f := setup(t)
	for _, p := range []string{"artifacts/p-1/50%.md", "projects/p/orchestrator/shared/50%.md"} {
		f.write(t, p, "literal percent")
		full := filepath.Join(f.data, filepath.FromSlash(p))
		urlPath := filepath.ToSlash(full)
		if !strings.HasPrefix(urlPath, "/") {
			urlPath = "/" + urlPath
		}
		for _, value := range []string{full, (&url.URL{Scheme: "file", Path: urlPath}).String()} {
			resolved, e := f.s.Resolve(context.Background(), Input{value: LocalPathTarget{Kind: "local-path", Value: value}})
			if e != nil || resolved.LocalPath != full || !strings.HasSuffix(resolved.CanonicalURI, "/50%25.md") {
				t.Fatalf("literal percent %q: %+v %v", value, resolved, e)
			}
		}
	}
	catalog, e := f.s.Catalog(context.Background(), "p", CatalogQuery{})
	if e != nil || catalog.Incomplete || len(catalog.Errors) != 0 || len(catalog.Items) != 5 {
		t.Fatalf("literal percent removed documents or made catalog incomplete: %+v %v", catalog, e)
	}
}

func TestCatalogSessionErrorsRespectSourceFilter(t *testing.T) {
	f := setup(t)
	f.store.sessionsErr = fmt.Errorf("synthetic session read failure")
	for _, tc := range []struct {
		source string
		want   []string
	}{
		{"worker", []string{"worker"}}, {"orchestrator", []string{"orchestrator"}},
		{"all", []string{"worker", "orchestrator"}}, {"shared", nil},
	} {
		t.Run(tc.source, func(t *testing.T) {
			result, e := f.s.Catalog(context.Background(), "p", CatalogQuery{Source: tc.source})
			if e != nil || len(result.Errors) != len(tc.want) || result.Incomplete != (len(tc.want) > 0) {
				t.Fatalf("source %s: %+v %v", tc.source, result, e)
			}
			for i, source := range tc.want {
				if result.Errors[i].Source != source || result.Errors[i].Code != "DOCUMENT_SOURCE_UNAVAILABLE" {
					t.Fatalf("unexpected source error: %+v", result.Errors[i])
				}
			}
		})
	}
}

func TestLocalPathsRejectEncodedSeparatorsAndEmptySegments(t *testing.T) {
	f := setup(t)
	f.write(t, "artifacts/p-1/report.md", "report")
	f.write(t, "artifacts/p-1/sub/nested.md", "nested")
	root := filepath.Join(f.data, "artifacts", "p-1")
	urlPath := filepath.ToSlash(root)
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	fileRoot := (&url.URL{Scheme: "file", Path: urlPath}).String()
	bad := []string{
		root + "//report.md", root + "/sub//nested.md", root + "/sub/./nested.md", root + "/sub/../report.md",
		fileRoot + "//report.md", fileRoot + "/sub//nested.md",
		fileRoot + "/sub/%2e%2e/report.md", fileRoot + "/sub/%252e%252e/report.md",
	}
	for _, encoded := range []string{"%2F", "%2f", "%5C", "%5c", "%252F", "%252f", "%255C", "%255c"} {
		bad = append(bad, fileRoot+encoded+"report.md", fileRoot+"/sub"+encoded+"nested.md")
	}
	for _, value := range bad {
		resolved, e := f.s.Resolve(context.Background(), Input{value: LocalPathTarget{Kind: "local-path", Value: value}})
		if e == nil {
			t.Errorf("accepted ambiguous local path %q: %+v", value, resolved)
		}
	}
}

func TestReadManagedContentRevalidatesSourceAndPreservesLiteralNames(t *testing.T) {
	f := setup(t)
	path := "mixed/%2e%2e%2fx%"
	f.write(t, "artifacts/p-1/"+path, "literal content")
	target := Input{value: ArtifactTarget{Kind: "artifact", ProjectID: "p", SessionID: "p-1", Path: path}}
	metadata, e := f.s.Resolve(context.Background(), target)
	if e != nil {
		t.Fatal(e)
	}
	content, e := f.s.Read(context.Background(), target, 1024, metadata.Fingerprint)
	if e != nil || string(content.Bytes) != "literal content" || content.Document.CanonicalURI != metadata.CanonicalURI {
		t.Fatalf("read: %+v %v", content, e)
	}
	if _, e = f.s.Read(context.Background(), target, 3, metadata.Fingerprint); e == nil {
		t.Fatal("read exceeded requested bound")
	}
	if _, e = f.s.Read(context.Background(), target, 1024, "outdated"); e == nil {
		t.Fatal("accepted stale content fingerprint")
	}
	f.store.sessions[0].Metadata.ArtifactDir = filepath.Join(f.data, "keys")
	if _, e = f.s.Read(context.Background(), target, 1024, metadata.Fingerprint); e == nil {
		t.Fatal("content read trusted previous metadata after root registration changed")
	}
}

func TestReadManagedContentRejectsReplacementAndCancellation(t *testing.T) {
	f := setup(t)
	target := Input{value: SharedTarget{Kind: "shared", ProjectID: "p", Path: "guide.md"}}
	metadata, e := f.s.Resolve(context.Background(), target)
	if e != nil {
		t.Fatal(e)
	}
	f.write(t, "keys/private", "private")
	if e = os.Remove(filepath.Join(f.shared, "guide.md")); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(filepath.Join(f.data, "keys/private"), filepath.Join(f.shared, "guide.md")); e != nil {
		t.Skip(e)
	}
	if _, e = f.s.Read(context.Background(), target, 1024, metadata.Fingerprint); e == nil {
		t.Fatal("content read followed replacement symlink")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = f.s.Read(ctx, target, 1024, ""); e == nil {
		t.Fatal("ignored cancelled content request")
	}
}
