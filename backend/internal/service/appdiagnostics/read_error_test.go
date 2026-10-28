package appdiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"
)

func TestSnapshotIOErrorsPreserveFilesAndRetryCleanup(t *testing.T) {
	for _, operation := range []string{"prune", "revoke"} {
		for _, fault := range []struct {
			name string
			err  error
		}{{"EIO", syscall.EIO}, {"EMFILE", syscall.EMFILE}} {
			t.Run(operation+"/"+fault.name, func(t *testing.T) {
				s, _, _, now, dir := fixture(t)
				g := grant(t, s)
				record, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(dir, "diagnostics", "snapshots", record.ID+".json")
				before, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				original := s.store.readContent
				s.store.readContent = func(file *os.File) ([]byte, error) {
					if filepath.Base(file.Name()) == record.ID+".json" {
						return []byte(`{"partial":`), &os.PathError{Op: "read", Path: "must-not-be-observed", Err: fault.err}
					}
					return original(file)
				}
				if operation == "prune" {
					err = s.store.prune(*now)
				} else {
					err = s.Revoke(context.Background(), g.Token)
				}
				if err != nil {
					t.Errorf("temporary read error disabled cleanup: %v", err)
				}
				if operation == "revoke" {
					rows, err := documents[Audit](s.store, "audit", nil)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, row := range rows {
						if row.value.GrantID == g.ID && row.value.Outcome == "revoked" {
							found = row.value.ReasonCode == "APP_DIAGNOSTICS_CLEANUP_PENDING"
						}
					}
					if !found {
						t.Error("effective revocation missing pending-cleanup audit")
					}
				}
				after, readErr := os.ReadFile(target)
				if readErr != nil || string(after) != string(before) {
					t.Fatalf("valid snapshot lost after temporary read failure: %v", readErr)
				}
				raw, err := s.store.root.ReadFile(filepath.Join("audit", retentionID+".json"))
				if err != nil {
					t.Fatal(err)
				}
				var summary map[string]json.RawMessage
				if err := json.Unmarshal(raw, &summary); err != nil {
					t.Fatal(err)
				}
				if string(summary["readErrorCount"]) != "1" {
					t.Fatalf("read failure not counted safely: %s", raw)
				}
				if strings.Contains(string(raw), "must-not-be-observed") || strings.Contains(string(raw), fault.err.Error()) {
					t.Fatal("raw I/O error leaked into summary")
				}
				s.store.readContent = original
				if err := s.store.prune(*now); err != nil {
					t.Fatal(err)
				}
				if operation == "revoke" {
					if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("revoked snapshot not cleaned after recovery: %v", err)
					}
				} else {
					if _, err := s.Read(context.Background(), g.Token, record.ID); err != nil {
						t.Fatalf("healthy snapshot lost after retry: %v", err)
					}
					log, err := s.Audits(context.Background(), g.Token)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := json.Marshal(log)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(raw), `"readErrorCount":1`) {
						t.Fatalf("read counter not visible through audit API: %s", raw)
					}
				}
			})
		}
	}
}

func TestInvalidSnapshotDeletionHasSeparateCounters(t *testing.T) {
	s, _, _, now, _ := fixture(t)
	g := grant(t, s)
	writeCleanupFixture(t, s.store, "audit", uuid.NewString()+".json", `{"unknownAuditSchema":true}`)
	writeCleanupFixture(t, s.store, "snapshots", uuid.NewString()+".json", `{"grantId":`)
	writeCleanupFixture(t, s.store, "snapshots", uuid.NewString()+".json", `{"grantId":""}`)
	if err := s.store.prune(*now); err != nil {
		t.Fatal(err)
	}
	log, err := s.Audits(context.Background(), g.Token)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(log)
	if err != nil {
		t.Fatal(err)
	}
	var counters map[string]json.RawMessage
	if err := json.Unmarshal(raw, &counters); err != nil {
		t.Fatal(err)
	}
	if string(counters["invalidSnapshotDeletedCount"]) != "2" || string(counters["skippedCount"]) != "1" {
		t.Fatalf("deletion and skip counters mixed: %s", raw)
	}
	entries, err := s.store.entries("snapshots")
	if err != nil || len(entries) != 0 {
		t.Fatalf("known invalid snapshots retained: %v %v", entries, err)
	}
}
