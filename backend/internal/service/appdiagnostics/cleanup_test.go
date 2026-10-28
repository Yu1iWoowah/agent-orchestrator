package appdiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func writeCleanupFixture(t *testing.T, s *diskStore, dir, name, content string) {
	t.Helper()
	f, err := createPrivateFile(s.root, filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartupPurgesAbandonedAuditTemp(t *testing.T) {
	s, _, _, now, dir := fixture(t)
	old := now.Add(-8 * 24 * time.Hour)
	managed := []string{uuid.NewString() + ".tmp", uuid.NewString() + ".tmp"}
	for _, name := range managed {
		writeCleanupFixture(t, s.store, "audit", name, "crash fixture")
	}
	if err := os.Chtimes(filepath.Join(dir, "diagnostics", "audit", managed[0]), old, old); err != nil {
		t.Fatal(err)
	}
	foreign := "not-a-uuid.tmp"
	future := uuid.NewString() + ".json"
	writeCleanupFixture(t, s.store, "audit", foreign, "unrecognized file")
	writeCleanupFixture(t, s.store, "audit", future, `{"futureSchema":true}`)
	outside := filepath.Join(dir, uuid.NewString()+".tmp")
	if err := os.WriteFile(outside, []byte("outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir, nil, nil, func() time.Time { return *now })
	if err != nil {
		t.Fatalf("unknown schema disabled service: %v", err)
	}
	defer reopened.Close()
	for _, name := range managed {
		if _, err := os.Lstat(filepath.Join(dir, "diagnostics", "audit", name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("abandoned audit temp retained: %s (%v)", name, err)
		}
	}
	for _, name := range []string{foreign, future} {
		if _, err := os.Lstat(filepath.Join(dir, "diagnostics", "audit", name)); err != nil {
			t.Errorf("unmanaged/unknown audit file removed: %s (%v)", name, err)
		}
	}
	raw, err := os.ReadFile(outside)
	if err != nil || string(raw) != "outside sentinel" {
		t.Fatalf("cleanup escaped managed audit: %q %v", raw, err)
	}
}

func TestSnapshotCleanupRemovesUnverifiableFiles(t *testing.T) {
	for _, operation := range []string{"revoke", "prune"} {
		t.Run(operation, func(t *testing.T) {
			s, _, _, now, dir := fixture(t)
			own := grant(t, s)
			other := grant(t, s)
			if _, err := s.Capture(context.Background(), own.Token, CaptureRequest{}); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(2 * time.Second)
			keep, err := s.Capture(context.Background(), other.Token, CaptureRequest{})
			if err != nil {
				t.Fatal(err)
			}
			bad := map[string]string{
				uuid.NewString() + ".json": `{"grantId":`,
				uuid.NewString() + ".json": `{"futureSchema":true}`,
				uuid.NewString() + ".json": `{"grantId":""}`,
				uuid.NewString() + ".json": `{"grantId":"not-a-grant"}`,
				"unattributable.json":      `{}`,
			}
			for _, grantID := range []string{"", "not-a-grant", uuid.Nil.String()} {
				id := uuid.NewString()
				raw, err := json.Marshal(Record{ID: id, GrantID: grantID, RequestID: uuid.NewString(), CreatedAt: *now, Snapshot: keep.Snapshot})
				if err != nil {
					t.Fatal(err)
				}
				bad[id+".json"] = string(raw)
			}
			var unreadable []string
			for _, kind := range []string{"identity", "schema"} {
				id := uuid.NewString()
				record := Record{ID: id, GrantID: own.ID, RequestID: uuid.NewString(), CreatedAt: *now, Snapshot: keep.Snapshot}
				if kind == "identity" {
					record.ID = uuid.NewString()
				} else {
					record.Snapshot.SchemaVersion = 0
				}
				raw, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				bad[id+".json"] = string(raw)
				unreadable = append(unreadable, id)
			}
			for name, content := range bad {
				writeCleanupFixture(t, s.store, "snapshots", name, content)
			}
			for _, id := range unreadable {
				if _, err := s.Read(context.Background(), own.Token, id); err == nil {
					t.Errorf("invalid snapshot envelope returned: %s", id)
				}
			}
			if operation == "revoke" {
				err = s.Revoke(context.Background(), own.Token)
			} else {
				err = s.store.prune(*now)
			}
			if err != nil {
				t.Fatal(err)
			}
			for name := range bad {
				if _, err := os.Lstat(filepath.Join(dir, "diagnostics", "snapshots", name)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("invalid snapshot retained: %s (%v)", name, err)
				}
			}
			if _, err := s.Read(context.Background(), other.Token, keep.ID); err != nil {
				t.Fatalf("another grant's valid snapshot lost: %v", err)
			}
		})
	}
}

func TestSnapshotCleanupUnlinksWithoutFollowingTargets(t *testing.T) {
	for _, operation := range []string{"revoke", "prune"} {
		for _, kind := range []string{"symlink-file", "symlink-directory", "hardlink", "unsafe-mode"} {
			t.Run(operation+"/"+kind, func(t *testing.T) {
				s, _, _, now, dir := fixture(t)
				g := grant(t, s)
				outside := t.TempDir()
				sentinel := filepath.Join(outside, "keep")
				if err := os.WriteFile(sentinel, []byte("outside sentinel"), 0o600); err != nil {
					t.Fatal(err)
				}
				name := uuid.NewString() + ".json"
				target := filepath.Join(dir, "diagnostics", "snapshots", name)
				switch kind {
				case "symlink-file":
					if err := os.Symlink(sentinel, target); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				case "symlink-directory":
					if err := os.Symlink(outside, target); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				case "hardlink":
					if err := os.Link(sentinel, target); err != nil {
						t.Skipf("hardlink unavailable: %v", err)
					}
				case "unsafe-mode":
					if goruntime.GOOS == "windows" {
						t.Skip("POSIX modes; native Windows ACL tests cover validation")
					}
					writeCleanupFixture(t, s.store, "snapshots", name, `{}`)
					if err := os.Chmod(target, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if operation == "revoke" {
					err = s.Revoke(context.Background(), g.Token)
				} else {
					err = s.store.prune(*now)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsafe snapshot link/file retained: %v", err)
				}
				raw, err := os.ReadFile(sentinel)
				if err != nil || string(raw) != "outside sentinel" {
					t.Fatalf("cleanup followed target: %q %v", raw, err)
				}
			})
		}
	}
}

func TestSnapshotCleanupRejectsSymlinkedDirectory(t *testing.T) {
	s, _, _, now, dir := fixture(t)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, uuid.NewString()+".json")
	if err := os.WriteFile(sentinel, []byte("outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(dir, "diagnostics", "snapshots")
	if err := os.Rename(managed, managed+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, managed); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := s.store.prune(*now); err == nil {
		t.Fatal("unsafe managed directory accepted")
	}
	raw, err := os.ReadFile(sentinel)
	if err != nil || string(raw) != "outside sentinel" {
		t.Fatalf("cleanup crossed managed directory: %q %v", raw, err)
	}
}

func TestTransientCaptureLookupIsAuditedSeparatelyFromDenial(t *testing.T) {
	s, rows, _, _, _ := fixture(t)
	g := grant(t, s)
	const privateError = "database busy at a private path"
	rows.err = errors.New(privateError)
	for i := 0; i < 2; i++ {
		_, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
		var typed *Error
		if !errors.As(err, &typed) || typed.Status != 503 {
			t.Fatalf("wrong transient result: %v", err)
		}
	}
	if s.deniedCount != 0 {
		t.Fatalf("transient failures counted as denial: %d", s.deniedCount)
	}
	rows.err = nil
	if _, err := s.Status(context.Background(), g.Token); err != nil {
		t.Fatalf("transient lookup revoked grant: %v", err)
	}
	if _, err := s.Capture(context.Background(), "ungranted-fixture", CaptureRequest{}); err == nil {
		t.Fatal("ungranted capture succeeded")
	}
	entries, err := s.store.entries("audit")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for _, entry := range entries {
		raw, err := s.store.root.ReadFile(filepath.Join("audit", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), g.Token) || strings.Contains(string(raw), privateError) {
			t.Fatal("audit leaked a token or query error")
		}
		var row Audit
		if err := s.store.read("audit", strings.TrimSuffix(entry.Name(), ".json"), &row); err != nil {
			t.Fatal(err)
		}
		if row.Outcome == "unavailable" && row.ReasonCode != "APP_DIAGNOSTICS_SESSION_UNAVAILABLE" {
			t.Fatalf("missing safe transient reason: %+v", row)
		}
		counts[row.Outcome] += row.Count
	}
	if counts["unavailable"] != 2 || counts["denied"] != 1 {
		t.Fatalf("wrong classified counts: %v", counts)
	}
}

func TestStartupAuditTempCleanupDoesNotFollowSymlinks(t *testing.T) {
	s, _, _, now, dir := fixture(t)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep")
	if err := os.WriteFile(sentinel, []byte("outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := uuid.NewString() + ".tmp"
	target := filepath.Join(dir, "diagnostics", "audit", name)
	if err := os.Symlink(outside, target); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	foreign := "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz.tmp"
	writeCleanupFixture(t, s.store, "audit", foreign, "not a UUID")
	nested := filepath.Join(dir, "diagnostics", "audit", uuid.NewString()+".tmp")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(nested, "keep")
	if err := os.WriteFile(marker, []byte("do not recurse"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir, nil, nil, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed temp symlink retained: %v", err)
	}
	for file, want := range map[string]string{sentinel: "outside sentinel", marker: "do not recurse", filepath.Join(dir, "diagnostics", "audit", foreign): "not a UUID"} {
		raw, err := os.ReadFile(file)
		if err != nil || string(raw) != want {
			t.Fatalf("cleanup followed/unexpectedly removed a target: %q %v", raw, err)
		}
	}
}

func TestSnapshotCleanupDoesNotRecurseIntoDirectories(t *testing.T) {
	for _, operation := range []string{"revoke", "prune"} {
		t.Run(operation, func(t *testing.T) {
			s, _, _, now, dir := fixture(t)
			g := grant(t, s)
			nested := filepath.Join(dir, "diagnostics", "snapshots", uuid.NewString()+".json")
			if err := os.Mkdir(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(nested, "keep")
			if err := os.WriteFile(marker, []byte("do not recurse"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "revoke" {
				err = s.Revoke(context.Background(), g.Token)
			} else {
				err = s.store.prune(*now)
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(marker)
			if err != nil || string(raw) != "do not recurse" {
				t.Fatalf("unexpected recursion: %q %v", raw, err)
			}
		})
	}
}

func TestCleanupRejectsPathsOutsideManagedSubdirectory(t *testing.T) {
	s, _, _, _, dir := fixture(t)
	g := grant(t, s)
	for _, request := range []struct{ dir, name string }{
		{"snapshots", string(filepath.Separator)},
		{"snapshots", filepath.Join("..", "grants", g.ID+".json")},
		{"grants", g.ID + ".json"},
		{"audit", filepath.Join("..", "grants", g.ID+".json")},
	} {
		if err := s.store.removeEntry(request.dir, request.name); err == nil {
			t.Fatal("cleanup crossed authority boundary")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "diagnostics", "grants", g.ID+".json")); err != nil {
		t.Fatalf("unrelated credential removed: %v", err)
	}
}
