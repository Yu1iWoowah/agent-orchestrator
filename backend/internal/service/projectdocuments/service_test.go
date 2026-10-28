package projectdocuments_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type capability struct{}

func (capability) Valid(_ domain.SessionID, token, verifier string) bool {
	return token == "actor-secret" && verifier == "actor-verifier"
}

type fixture struct {
	service *docs.Service
	db      *sqlite.Store
	root    string
	dbDir   string
	owner   domain.SessionRecord
	actor   docs.Actor
}

func setup(t *testing.T) fixture {
	t.Helper()
	dbDir := t.TempDir()
	db := sqlitetest.MustOpenAt(t, dbDir)
	ctx := context.Background()
	if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "demo", Path: t.TempDir(), RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	owner, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "demo", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{ControllerGeneration: "epoch-1", ProviderConversationID: "native-1", BrowserCapabilityVerifier: "actor-verifier"}})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	return fixture{service: docs.New(docs.Deps{Store: db, DataDir: root, Capabilities: capability{}}), db: db, root: root, dbDir: dbDir, owner: owner, actor: docs.Actor{SessionID: owner.ID, Capability: "actor-secret"}}
}
func TestOwnerWritesAreConfinedAndOptimistic(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	info, err := f.service.Info(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if info.SharedDir != filepath.Join(f.root, "projects", "demo", "orchestrator", "shared") {
		t.Fatalf("unexpected shared root: %s", info.SharedDir)
	}
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	write := docs.WriteInput{Actor: f.actor, Lease: grant, Path: "handoff/orchestrator-handoff.md", Content: "# first", ExpectedHash: docs.MissingHash}
	file, err := f.service.Write(ctx, "demo", write)
	if err != nil {
		t.Fatal(err)
	}
	if file.Content != "# first" || file.Hash == "" {
		t.Fatalf("file=%+v", file)
	}
	write.Content = "# stale"
	if _, err = f.service.Write(ctx, "demo", write); err == nil {
		t.Fatal("stale create overwrote an existing file")
	}
	for _, path := range []string{"../outside", "/etc/passwd", "handoff/../../outside", "rules\\escape", ""} {
		write.Path = path
		if _, err = f.service.Write(ctx, "demo", write); err == nil {
			t.Fatalf("accepted escaping path %q", path)
		}
	}
	got, err := f.service.Read(ctx, "demo", "handoff/orchestrator-handoff.md")
	if err != nil || got.Content != "# first" {
		t.Fatalf("read=%+v err=%v", got, err)
	}
	// Neither read nor link discovery depends on the source remaining alive.
	f.owner.IsTerminated = true
	if err = f.db.UpdateSession(ctx, f.owner); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Read(ctx, "demo", "handoff/orchestrator-handoff.md"); err != nil {
		t.Fatal(err)
	}
}
func TestWorkerAndForgedCapabilitiesCannotAcquire(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	bad := f.actor
	bad.Capability = "forged"
	if _, err := f.service.Acquire(ctx, "demo", bad); err == nil {
		t.Fatal("forged actor acquired lease")
	}
	worker, err := f.db.CreateSession(ctx, domain.SessionRecord{ProjectID: "demo", Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{BrowserCapabilityVerifier: "actor-verifier"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Acquire(ctx, "demo", docs.Actor{SessionID: worker.ID, Capability: "actor-secret"}); err == nil {
		t.Fatal("worker acquired orchestrator lease")
	}
}
func TestFrozenLeaseSurvivesRestartAndTransfersOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.Freeze(ctx, "demo", f.actor, grant); err != nil {
		t.Fatal(err)
	}
	restarted := docs.New(docs.Deps{Store: f.db, DataDir: f.root, Capabilities: capability{}})
	if _, err = restarted.Acquire(ctx, "demo", f.actor); err == nil {
		t.Fatal("restart silently unfroze lease")
	}
	if _, err = restarted.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: grant, Path: "backlog/a.md", ExpectedHash: docs.MissingHash, Content: "stale"}); err == nil {
		t.Fatal("frozen writer changed file")
	}
	f.owner.IsTerminated = true
	if err = f.db.UpdateSession(ctx, f.owner); err != nil {
		t.Fatal(err)
	}
	successor, err := f.db.CreateSession(ctx, domain.SessionRecord{ProjectID: "demo", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{ControllerGeneration: "epoch-2", ProviderConversationID: "native-2", BrowserCapabilityVerifier: "actor-verifier"}})
	if err != nil {
		t.Fatal(err)
	}
	actor := docs.Actor{SessionID: successor.ID, Capability: "actor-secret"}
	next, err := restarted.Transfer(ctx, "demo", grant, actor)
	if err != nil {
		t.Fatal(err)
	}
	if next.Epoch <= grant.Epoch {
		t.Fatal("lease epoch did not advance")
	}
	if _, err = restarted.Transfer(ctx, "demo", grant, actor); err == nil {
		t.Fatal("stale transfer succeeded twice")
	}
	if _, err = restarted.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: grant, Path: "backlog/a.md", ExpectedHash: docs.MissingHash, Content: "old"}); err == nil {
		t.Fatal("old source wrote after transfer")
	}
	if _, err = restarted.Write(ctx, "demo", docs.WriteInput{Actor: actor, Lease: next, Path: "backlog/a.md", ExpectedHash: docs.MissingHash, Content: "new"}); err != nil {
		t.Fatal(err)
	}
}
func TestSymlinkIsNotAProjectDocument(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	g, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.service.Info(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err = os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(info.SharedDir, "leak.md")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if _, err = f.service.Read(ctx, "demo", "leak.md"); err == nil {
		t.Fatal("read symlink")
	}
	if _, err = f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: g, Path: "leak.md", ExpectedHash: docs.MissingHash, Content: "overwrite"}); err == nil {
		t.Fatal("wrote symlink")
	}
	b, err := os.ReadFile(outside)
	if err != nil || string(b) != "secret" {
		t.Fatalf("outside changed: %q %v", b, err)
	}
}
func TestAdoptionIsScopedAndDoesNotOverwrite(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	g, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(f.root, "artifacts", string(f.owner.ID))
	for _, name := range []string{"handoff/orchestrator-handoff.md", "rules/orchestrator-rules.md", "backlog/list.md", "architecture/index.md", "tools/not-living.sh"} {
		p := filepath.Join(legacy, name)
		if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(p, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := f.service.Adopt(ctx, "demo", docs.AdoptInput{Actor: f.actor, Lease: g, SourceSessionID: f.owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 4 {
		t.Fatalf("adopted=%+v", result)
	}
	for _, entry := range result.Files {
		if strings.HasPrefix(entry.Path, "tools/") {
			t.Fatal("imported tools")
		}
	}
	if _, err = f.service.Read(ctx, "demo", "tools/not-living.sh"); err == nil {
		t.Fatal("tools copied")
	}
	if _, err = f.service.Adopt(ctx, "demo", docs.AdoptInput{Actor: f.actor, Lease: g, SourceSessionID: f.owner.ID}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(legacy, "rules/orchestrator-rules.md"))
	if err != nil || string(content) != "rules/orchestrator-rules.md" {
		t.Fatal("legacy changed")
	}
}

func TestSameControllerAcquisitionIsIdempotent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("the same owner invalidated its own in-flight lease")
	}
	if _, err = f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: first, Path: "rules/r.md", ExpectedHash: docs.MissingHash, Content: "rules"}); err != nil {
		t.Fatal(err)
	}
}
func TestPendingAdoptionDoesNotExposePartialFiles(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.service.Acquire(ctx, "demo", f.actor); err != nil {
		t.Fatal(err)
	}
	state, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	state.Adoption = &domain.ProjectDocumentAdoption{SourceSessionID: f.owner.ID, Complete: false}
	if ok, err := f.db.SaveProjectDocumentState(ctx, "demo", state); err != nil || !ok {
		t.Fatalf("seed pending=%v %v", ok, err)
	}
	if err = os.WriteFile(filepath.Join(state.SharedDir, "partial.md"), []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := f.service.Info(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Files) != 0 {
		t.Fatal("partial adoption was advertised as current project files")
	}
	if _, err = f.service.Read(ctx, "demo", "partial.md"); err == nil {
		t.Fatal("read partial adoption")
	}
}

func legacyFixture(t *testing.T, f fixture) string {
	t.Helper()
	root := filepath.Join(f.root, "artifacts", string(f.owner.ID))
	for _, p := range []string{"handoff/doc.md", "rules/main.md", "rules/.bak-old.md", "backlog/list.md", "architecture/model.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte("# "+p), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func TestLivingFileAdoptionManifestAndRetry(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	legacy := legacyFixture(t, f)
	content := "[model](../architecture/model.md)\nOld: " + legacy + "/rules/main.md\n"
	if err := os.WriteFile(filepath.Join(legacy, "handoff/doc.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	in := docs.AdoptInput{Actor: f.actor, Lease: grant, SourceSessionID: f.owner.ID}
	got, err := f.service.Adopt(ctx, "demo", in)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Complete || got.ManifestHash == "" {
		t.Fatalf("adoption=%+v", got)
	}
	b, err := os.ReadFile(filepath.Join(f.root, "projects/demo/orchestrator/adoptions", string(f.owner.ID), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SourceRoot         string `json:"sourceRoot"`
		SourceRootIdentity string `json:"sourceRootIdentity"`
		Files              []struct {
			Backup          bool   `json:"backup"`
			SourceHash      string `json:"sourceHash"`
			DestinationHash string `json:"destinationHash"`
		} `json:"files"`
		Diagnostics   []string          `json:"diagnostics"`
		LegacyPathMap map[string]string `json:"legacyPathMap"`
	}
	if err = json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SourceRoot != legacy || manifest.SourceRootIdentity == "" || len(manifest.Diagnostics) == 0 || len(manifest.LegacyPathMap) != 5 {
		t.Fatalf("manifest=%s", b)
	}
	backup := false
	for _, entry := range manifest.Files {
		backup = backup || entry.Backup
		if entry.SourceHash == "" || entry.SourceHash != entry.DestinationHash {
			t.Fatal("hash evidence missing")
		}
	}
	if !backup {
		t.Fatal("backup marker missing")
	}
	if err = os.WriteFile(filepath.Join(legacy, "rules/main.md"), []byte("legacy later"), 0600); err != nil {
		t.Fatal(err)
	}
	retry, err := f.service.Adopt(ctx, "demo", in)
	if err != nil || retry.ManifestHash != got.ManifestHash {
		t.Fatalf("retry=%+v %v", retry, err)
	}
	file, err := f.service.Read(ctx, "demo", "rules/main.md")
	if err != nil || file.Content == "legacy later" {
		t.Fatal("completed adoption reimported later legacy changes")
	}
}
func TestAdoptionRejectsMissingAssetsAndConflicts(t *testing.T) {
	for _, mode := range []string{"asset", "escape", "conflict", "symlink", "missing-prefix"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			legacy := legacyFixture(t, f)
			g, err := f.service.Acquire(ctx, "demo", f.actor)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "asset":
				err = os.WriteFile(filepath.Join(legacy, "handoff/doc.md"), []byte("![missing](../architecture/missing.png)"), 0600)
			case "escape":
				err = os.WriteFile(filepath.Join(legacy, "handoff/doc.md"), []byte("[escape](../../outside.md)"), 0600)
			case "symlink":
				err = os.Symlink(filepath.Join(legacy, "rules/main.md"), filepath.Join(legacy, "rules/link.md"))
				if err != nil {
					t.Skipf("symlink fixture unavailable: %v", err)
				}
			case "missing-prefix":
				err = os.RemoveAll(filepath.Join(legacy, "architecture"))
			case "conflict":
				_, err = f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: g, Path: "rules/main.md", Content: "existing", ExpectedHash: docs.MissingHash})
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.service.Adopt(ctx, "demo", docs.AdoptInput{Actor: f.actor, Lease: g, SourceSessionID: f.owner.ID}); err == nil {
				t.Fatalf("accepted %s", mode)
			}
			state, err := f.db.GetProjectDocumentState(ctx, "demo")
			if err != nil {
				t.Fatal(err)
			}
			if state.Adoption != nil && state.Adoption.Complete {
				t.Fatal("failed adoption is complete")
			}
			if mode == "conflict" {
				b, e := os.ReadFile(filepath.Join(state.SharedDir, "rules/main.md"))
				if e != nil || string(b) != "existing" {
					t.Fatal("conflict overwritten")
				}
			}
		})
	}
}

func TestControllerRestartFencesOldLease(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	old, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	f.owner.Metadata.ControllerGeneration = "epoch-new"
	if err = f.db.UpdateSession(ctx, f.owner); err != nil {
		t.Fatal(err)
	}
	next, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	if next.Epoch <= old.Epoch {
		t.Fatal("epoch was reused")
	}
	if _, err = f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: old, Path: "rules/stale.md", Content: "old", ExpectedHash: docs.MissingHash}); err == nil {
		t.Fatal("old controller wrote")
	}
}
func TestFreezeSerializesConcurrentWrites(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	g, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			_, _ = f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: g, Path: "rules/race.md", Content: "value", ExpectedHash: docs.MissingHash})
		})
	}
	if err = f.service.Freeze(ctx, "demo", f.actor, g); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	info, err := f.service.Info(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if info.Lease == nil || !info.Lease.Frozen {
		t.Fatal("freeze not durable")
	}
	for i := 0; i < 5; i++ {
		if _, err = f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: g, Path: "rules/late.md", Content: "late", ExpectedHash: docs.MissingHash}); err == nil {
			t.Fatal("write after freeze")
		}
	}
}
func TestPendingAdoptionResumesAfterRestartAndRejectsSourceReplacement(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "replaced-file"}[replace], func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			legacy := legacyFixture(t, f)
			g, err := f.service.Acquire(ctx, "demo", f.actor)
			if err != nil {
				t.Fatal(err)
			}
			in := docs.AdoptInput{Actor: f.actor, Lease: g, SourceSessionID: f.owner.ID}
			result, err := f.service.Adopt(ctx, "demo", in)
			if err != nil {
				t.Fatal(err)
			}
			state, err := f.db.GetProjectDocumentState(ctx, "demo")
			if err != nil {
				t.Fatal(err)
			}
			state.Adoption.Complete = false
			state.Adoption.ManifestHash = ""
			if ok, e := f.db.SaveProjectDocumentState(ctx, "demo", state); e != nil || !ok {
				t.Fatalf("pending seed: %v", e)
			}
			if replace {
				p := filepath.Join(legacy, "rules/main.md")
				b, e := os.ReadFile(p)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(p+".replacement", b, 0600); e != nil {
					t.Fatal(e)
				}
				if e = os.Rename(p+".replacement", p); e != nil {
					t.Fatal(e)
				}
			}
			restarted := docs.New(docs.Deps{Store: f.db, DataDir: f.root, Capabilities: capability{}})
			got, err := restarted.Adopt(ctx, "demo", in)
			if replace {
				if err == nil {
					t.Fatal("accepted new source identity")
				}
				return
			}
			if err != nil || !got.Complete || got.ManifestHash != result.ManifestHash {
				t.Fatalf("resume=%+v %v", got, err)
			}
		})
	}
}

type rotatingStore struct {
	*sqlite.Store
	owner   domain.SessionRecord
	rotated bool
}

func (s *rotatingStore) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	record, ok, err := s.Store.GetSession(ctx, id)
	if err == nil && ok && id == s.owner.ID && !s.rotated {
		s.rotated = true
		s.owner.Metadata.ControllerGeneration = "rotated-during-write"
		if err := s.UpdateSession(ctx, s.owner); err != nil {
			return record, ok, err
		}
	}
	return record, ok, err
}
func TestWriterIsFencedAgainBeforePublication(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	service := docs.New(docs.Deps{Store: &rotatingStore{Store: f.db, owner: f.owner}, DataDir: f.root, Capabilities: capability{}})
	if _, err = service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: grant, Path: "rules/retired.md", Content: "stale", ExpectedHash: docs.MissingHash}); err == nil {
		t.Fatal("controller changed during the write but its old bytes were published")
	}
	if _, err = f.service.Read(ctx, "demo", "rules/retired.md"); err == nil {
		t.Fatal("stale bytes became visible")
	}
}

func TestProjectRootsRemainDistinctOnCaseInsensitiveFilesystems(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.db.UpsertProject(ctx, domain.ProjectRecord{ID: "Demo", Path: t.TempDir(), RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	first, err := f.service.Info(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.service.Info(ctx, "Demo")
	if err != nil {
		t.Fatal(err)
	}
	if strings.EqualFold(first.SharedDir, second.SharedDir) {
		t.Fatalf("case-folded roots collide: %s / %s", first.SharedDir, second.SharedDir)
	}
	upper := f.owner
	upper.ID = ""
	upper.ProjectID = "Demo"
	owner, err := f.db.CreateSession(ctx, upper)
	if err != nil {
		t.Fatal(err)
	}
	actor := docs.Actor{SessionID: owner.ID, Capability: "actor-secret"}
	grant, err := f.service.Acquire(ctx, "Demo", actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Write(ctx, "Demo", docs.WriteInput{Actor: actor, Lease: grant, Path: "rules/upper.md", Content: "upper", ExpectedHash: docs.MissingHash}); err != nil {
		t.Fatal(err)
	}
	upperFixture := f
	upperFixture.owner = owner
	upperFixture.actor = actor
	legacyFixture(t, upperFixture)
	if _, err = f.service.Adopt(ctx, "Demo", docs.AdoptInput{Actor: actor, Lease: grant, SourceSessionID: owner.ID}); err != nil {
		t.Fatal(err)
	}

}

type adoptionFaultStore struct {
	*sqlite.Store
	lookups, saves          int
	interruptAt, failSaveAt int
	mutateAt                int
	mutate                  func() error
}

func (s *adoptionFaultStore) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	s.lookups++
	if s.lookups == s.interruptAt {
		return domain.SessionRecord{}, false, context.Canceled
	}
	if s.lookups == s.mutateAt {
		if err := s.mutate(); err != nil {
			return domain.SessionRecord{}, false, err
		}
	}
	return s.Store.GetSession(ctx, id)
}
func (s *adoptionFaultStore) SaveProjectDocumentState(ctx context.Context, id domain.ProjectID, state domain.ProjectDocumentState) (bool, error) {
	s.saves++
	if s.saves == s.failSaveAt {
		return false, &os.PathError{Op: "write", Path: "adoption-state", Err: syscall.ENOSPC}
	}
	return s.Store.SaveProjectDocumentState(ctx, id, state)
}
func TestAdoptionFailuresNeverActivatePartialCopies(t *testing.T) {
	for _, mode := range []string{"copy-interrupted", "completion-enospc", "source-mutated"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			legacy := legacyFixture(t, f)
			g, err := f.service.Acquire(ctx, "demo", f.actor)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(filepath.Join(legacy, "rules/main.md"))
			if err != nil {
				t.Fatal(err)
			}
			fault := &adoptionFaultStore{Store: f.db}
			switch mode {
			case "copy-interrupted":
				fault.interruptAt = 4
			case "completion-enospc":
				fault.failSaveAt = 2
			case "source-mutated":
				fault.mutateAt = 4
				fault.mutate = func() error {
					return os.WriteFile(filepath.Join(legacy, "rules/main.md"), []byte("changed during copy"), 0600)
				}
			}
			service := docs.New(docs.Deps{Store: fault, DataDir: f.root, Capabilities: capability{}})
			in := docs.AdoptInput{Actor: f.actor, Lease: g, SourceSessionID: f.owner.ID}
			if _, err = service.Adopt(ctx, "demo", in); err == nil {
				t.Fatal("fault did not interrupt adoption")
			}
			state, err := f.db.GetProjectDocumentState(ctx, "demo")
			if err != nil {
				t.Fatal(err)
			}
			if state.Adoption == nil || state.Adoption.Complete {
				t.Fatal("failed operation activated the shared root")
			}
			if _, err = f.service.Read(ctx, "demo", "handoff/doc.md"); err == nil {
				t.Fatal("partial copy became readable")
			}
			if _, err = os.Stat(filepath.Join(state.SharedDir, "handoff/doc.md")); err != nil {
				t.Fatal("fixture did not reach a partial publication")
			}
			restarted := docs.New(docs.Deps{Store: f.db, DataDir: f.root, Capabilities: capability{}})
			if mode == "source-mutated" {
				if _, err = restarted.Adopt(ctx, "demo", in); err == nil {
					t.Fatal("retry mixed source versions")
				}
				if err = os.WriteFile(filepath.Join(legacy, "rules/main.md"), original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := restarted.Adopt(ctx, "demo", in)
			if err != nil || !result.Complete {
				t.Fatalf("recovery=%+v %v", result, err)
			}
			after, err := os.ReadFile(filepath.Join(legacy, "rules/main.md"))
			if err != nil || string(after) != string(original) {
				t.Fatal("legacy bytes were modified")
			}
		})
	}
}
func TestAdoptionNeverReplacesConflictingImmutableManifest(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	legacyFixture(t, f)
	g, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	in := docs.AdoptInput{Actor: f.actor, Lease: g, SourceSessionID: f.owner.ID}
	if _, err = f.service.Adopt(ctx, "demo", in); err != nil {
		t.Fatal(err)
	}
	state, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	state.Adoption.Complete = false
	state.Adoption.ManifestHash = ""
	if ok, e := f.db.SaveProjectDocumentState(ctx, "demo", state); e != nil || !ok {
		t.Fatalf("pending=%v", e)
	}
	p := filepath.Join(f.root, "projects/demo/orchestrator/adoptions", string(f.owner.ID), "manifest.json")
	if err = os.WriteFile(p, []byte("immutable conflicting evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Adopt(ctx, "demo", in); err == nil {
		t.Fatal("manifest conflict was accepted")
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "immutable conflicting evidence" {
		t.Fatal("conflicting immutable evidence overwritten")
	}
}

func TestLargeAdoptionManifestRecoversAfterCompletionFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.service.Info(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(f.root, "artifacts", string(f.owner.ID))
	for _, prefix := range []string{"handoff", "rules", "backlog", "architecture"} {
		if err = os.MkdirAll(filepath.Join(legacy, prefix), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.MkdirAll(filepath.Join(info.SharedDir, "backlog"), 0o700); err != nil {
		t.Fatal(err)
	}
	const files = 2048
	firstPath := ""
	for i := 0; i < files; i++ {
		p := filepath.Join("backlog", strings.Repeat("p", 190)+fmt.Sprintf("-%04d.txt", i))
		if i == 0 {
			firstPath = filepath.ToSlash(p)
		}
		// Matching destination files are a supported adoption input. Reusing them
		// keeps this receipt-size regression focused on publication and recovery.
		for _, root := range []string{legacy, info.SharedDir} {
			if err = os.WriteFile(filepath.Join(root, p), []byte("tiny"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	fault := &adoptionFaultStore{Store: f.db, failSaveAt: 2}
	interrupted := docs.New(docs.Deps{Store: fault, DataDir: f.root, Capabilities: capability{}})
	in := docs.AdoptInput{Actor: f.actor, Lease: grant, SourceSessionID: f.owner.ID}
	if _, err = interrupted.Adopt(ctx, "demo", in); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("completion failure=%v", err)
	}
	manifestPath := filepath.Join(f.root, "projects/demo/orchestrator/adoptions", string(f.owner.ID), "manifest.json")
	receipt, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt) <= docs.MaxFileBytes {
		t.Fatalf("receipt fixture is only %d bytes", len(receipt))
	}
	t.Logf("published receipt: %d bytes", len(receipt))
	state, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if state.Adoption == nil || state.Adoption.Complete {
		t.Fatal("failed completion activated adoption")
	}
	if _, err = f.service.Read(ctx, "demo", firstPath); err == nil {
		t.Fatal("pending adoption became readable")
	}
	restarted := docs.New(docs.Deps{Store: f.db, DataDir: f.root, Capabilities: capability{}})
	result, err := restarted.Adopt(ctx, "demo", in)
	if err != nil {
		t.Fatalf("recovery failed for %d-byte receipt: %v", len(receipt), err)
	}
	digest := sha256.Sum256(receipt)
	if !result.Complete || len(result.Files) != files || result.ManifestHash != fmt.Sprintf("%x", digest) {
		t.Fatal("recovery did not activate the original receipt")
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(after, receipt) {
		t.Fatal("recovery replaced the published receipt")
	}
	// A longer receipt with an identical prefix must still be rejected rather
	// than treated as a match after reading only the expected prefix.
	state, err = f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	state.Adoption.Complete = false
	if ok, e := f.db.SaveProjectDocumentState(ctx, "demo", state); e != nil || !ok {
		t.Fatalf("pending receipt fixture: %v", e)
	}
	corrupted := append(bytes.Clone(receipt), '!')
	if err = os.WriteFile(manifestPath, corrupted, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Adopt(ctx, "demo", in); err == nil {
		t.Fatal("accepted receipt with extra bytes")
	}
	after, err = os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(after, corrupted) {
		t.Fatal("conflicting receipt was overwritten")
	}
}

type finalAuthorizationRotationStore struct {
	*sqlite.Store
	calls int
	owner domain.SessionRecord
}

func (s *finalAuthorizationRotationStore) ListSessions(ctx context.Context, id domain.ProjectID) ([]domain.SessionRecord, error) {
	s.calls++
	if s.calls == 2 {
		s.owner.Metadata.ControllerGeneration = "new-controller-before-publish"
		s.owner.Metadata.BrowserCapabilityVerifier = "new-controller-verifier"
		if err := s.UpdateSession(ctx, s.owner); err != nil {
			return nil, err
		}
	}
	return s.Store.ListSessions(ctx, id)
}
func TestWriterRejectsRotationInsideFinalAuthorization(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	g, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	hook := &finalAuthorizationRotationStore{Store: f.db, owner: f.owner}
	svc := docs.New(docs.Deps{Store: hook, DataDir: f.root, Capabilities: capability{}})
	result, writeErr := svc.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: g, Path: "rules/rotation.md", Content: "old-controller-published", ExpectedHash: docs.MissingHash})
	current, _, err := f.db.GetSession(ctx, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("final authorization list calls=%d current generation=%s current verifier=%s write error=%v content=%q", hook.calls, current.Metadata.ControllerGeneration, current.Metadata.BrowserCapabilityVerifier, writeErr, result.Content)
	if writeErr == nil {
		t.Error("stale controller published after durable controller rotation")
	}
	if _, err := f.service.Read(ctx, "demo", "rules/rotation.md"); err == nil {
		t.Error("stale file exists")
	}
}

func TestPublicationReservationExcludesControllerRotation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.service.Acquire(ctx, "demo", f.actor); err != nil {
		t.Fatal(err)
	}
	state, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	// A separate store has its own Go mutex and connection pools. Only SQLite's
	// writer reservation can fence this controller update.
	other, err := sqlite.OpenPreMigrated(f.dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	started, finished := make(chan struct{}), make(chan error, 1)
	published, err := f.db.WithProjectDocumentPublication(ctx, "demo", f.owner, state.Revision, func(context.Context) error {
		go func() {
			close(started)
			rotated := f.owner
			rotated.Metadata.ControllerGeneration = "after-publication"
			rotated.Metadata.BrowserCapabilityVerifier = "rotated-verifier"
			finished <- other.UpdateSession(ctx, rotated)
		}()
		<-started
		select {
		case err := <-finished:
			return fmt.Errorf("controller rotation crossed active publication: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
		current, _, err := other.GetSession(ctx, f.owner.ID)
		if err != nil {
			return err
		}
		if current.ControllerOwner() != f.owner.ControllerOwner() {
			return errors.New("controller rotated before publication finished")
		}
		return nil
	})
	if err != nil || !published {
		t.Fatalf("publication=%v err=%v", published, err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not resume after publication")
	}
	called := false
	published, err = f.db.WithProjectDocumentPublication(ctx, "demo", f.owner, state.Revision, func(context.Context) error { called = true; return nil })
	if err != nil || published || called {
		t.Fatalf("stale reservation published=%v callback=%v err=%v", published, called, err)
	}
}

func TestAdoptionReceiptRejectsRotationInsideFinalAuthorization(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	legacyFixture(t, f)
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	input := docs.AdoptInput{Actor: f.actor, Lease: grant, SourceSessionID: f.owner.ID}
	if _, err := f.service.Adopt(ctx, "demo", input); err != nil {
		t.Fatal(err)
	}
	state, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	state.Adoption.Complete = false
	state.Adoption.ManifestHash = ""
	if ok, err := f.db.SaveProjectDocumentState(ctx, "demo", state); err != nil || !ok {
		t.Fatalf("reset pending: %v", err)
	}
	receipt := filepath.Join(f.root, "projects/demo/orchestrator/adoptions", string(f.owner.ID), "manifest.json")
	if err := os.Remove(receipt); err != nil {
		t.Fatal(err)
	}
	// All files already match, so the second authorization is the receipt publish.
	hook := &finalAuthorizationRotationStore{Store: f.db, owner: f.owner}
	svc := docs.New(docs.Deps{Store: hook, DataDir: f.root, Capabilities: capability{}})
	if _, err := svc.Adopt(ctx, "demo", input); err == nil {
		t.Fatal("stale controller published adoption receipt")
	}
	if hook.calls != 2 {
		t.Fatalf("unexpected authorization calls: %d", hook.calls)
	}
	if _, err := os.Stat(receipt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt exists: %v", err)
	}
	state, err = f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil || state.Adoption.Complete {
		t.Fatalf("adoption activated: %+v err=%v", state.Adoption, err)
	}
}
