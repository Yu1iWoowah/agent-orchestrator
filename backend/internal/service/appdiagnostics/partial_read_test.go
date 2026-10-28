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
	"time"

	"github.com/google/uuid"
)

func failDocumentRead(s *diskStore, id string, fault error) func() {
	original := s.readContent
	s.readContent = func(file *os.File) ([]byte, error) {
		if filepath.Base(file.Name()) == id+".json" {
			return []byte(`{"partial":`), &os.PathError{Op: "read", Path: "private-path", Err: fault}
		}
		return original(file)
	}
	return func() { s.readContent = original }
}

func TestResetAndPruneContinueWithUnreadableAudit(t *testing.T) {
	for _, fault := range []struct {
		name string
		err  error
	}{{"EIO", syscall.EIO}, {"EMFILE", syscall.EMFILE}} {
		t.Run(fault.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := openStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.root.Close()
			now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
			blocked, accepted, expired := uuid.NewString(), uuid.NewString(), uuid.NewString()
			for _, row := range []Audit{{RequestID: blocked, At: now, Outcome: "accepted"}, {RequestID: accepted, At: now, Outcome: "accepted"}, {RequestID: expired, At: now.Add(-8 * 24 * time.Hour), Outcome: "completed"}} {
				if err := store.write("audit", row.RequestID, row); err != nil {
					t.Fatal(err)
				}
			}
			original, err := os.ReadFile(filepath.Join(dir, "diagnostics", "audit", blocked+".json"))
			if err != nil {
				t.Fatal(err)
			}
			restore := failDocumentRead(store, blocked, fault.err)
			if err := store.reset(now); err != nil {
				t.Fatalf("single audit read failure disabled startup: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(dir, "diagnostics", "audit", blocked+".json"))
			if err != nil || string(after) != string(original) {
				t.Fatalf("unreadable audit changed: %v", err)
			}
			var row Audit
			if err := store.read("audit", accepted, &row); err != nil || row.Outcome != "interrupted" {
				t.Fatalf("readable accepted audit not interrupted: %+v %v", row, err)
			}
			for i := 0; i < 2; i++ {
				if i == 1 {
					if err := store.write("audit", expired, Audit{RequestID: expired, At: now.Add(-8 * 24 * time.Hour), Outcome: "completed"}); err != nil {
						t.Fatal(err)
					}
					if err := store.prune(now); err != nil {
						t.Fatalf("persistent audit fault disabled prune: %v", err)
					}
				}
				if _, err := store.root.Stat(filepath.Join("audit", expired+".json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("readable expired audit retained: %v", err)
				}
			}
			if _, ok := store.auditTimes[accepted]; !ok {
				t.Fatal("readable audit index not rebuilt")
			}
			var summary Audit
			if err := store.read("audit", retentionID, &summary); err != nil || summary.ReadErrorCount == 0 {
				t.Fatalf("read error not counted: %+v %v", summary, err)
			}
			restore()
		})
	}
}

func TestCaptureCommitsDespiteOtherDocumentReadFault(t *testing.T) {
	for _, dir := range []string{"snapshots", "audit"} {
		t.Run(dir, func(t *testing.T) {
			s, _, _, now, _ := fixture(t)
			g := grant(t, s)
			first, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
			if err != nil {
				t.Fatal(err)
			}
			id := first.ID
			if dir == "audit" {
				id = first.RequestID
			}
			restore := failDocumentRead(s.store, id, syscall.EIO)
			defer restore()
			*now = now.Add(2 * time.Second)
			second, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
			if err != nil {
				t.Fatalf("unrelated document read failure rolled back capture: %v", err)
			}
			var retained Record
			if err := s.store.read("snapshots", second.ID, &retained); err != nil {
				t.Fatalf("committed snapshot missing: %v", err)
			}
			var audit Audit
			if err := s.store.read("audit", second.RequestID, &audit); err != nil || audit.Outcome != "completed" {
				t.Fatalf("capture audit disagrees with commit: %+v %v", audit, err)
			}
		})
	}
}

func TestCapturePruneWriteFailureRewritesCompletedAudit(t *testing.T) {
	s, _, _, _, _ := fixture(t)
	g := grant(t, s)
	writeCleanupFixture(t, s.store, "snapshots", uuid.NewString()+".json", `{"invalid":`)
	original := s.store.writeContent
	var captured Record
	s.store.writeContent = func(file *os.File, data []byte) (int, error) {
		if strings.Contains(string(data), `"outcome":"retention"`) {
			return 0, syscall.EIO
		}
		var record Record
		if json.Unmarshal(data, &record) == nil && record.ID != "" {
			captured = record
		}
		return original(file, data)
	}
	defer func() { s.store.writeContent = original }()
	if _, err := s.Capture(context.Background(), g.Token, CaptureRequest{}); err == nil {
		t.Fatal("real retention write failure returned success")
	}
	if captured.ID == "" {
		t.Fatal("fixture did not reach snapshot commit")
	}
	if _, err := s.store.root.Stat(filepath.Join("snapshots", captured.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed capture not rolled back: %v", err)
	}
	var audit Audit
	if err := s.store.read("audit", captured.RequestID, &audit); err != nil || audit.Outcome != "storage_failed" {
		t.Fatalf("rollback left completed audit: %+v %v", audit, err)
	}
}

func TestListAndAuditsRemainAvailableWithPersistentReadFault(t *testing.T) {
	for _, dir := range []string{"snapshots", "audit"} {
		t.Run(dir, func(t *testing.T) {
			s, _, _, now, _ := fixture(t)
			g := grant(t, s)
			first, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
			if err != nil {
				t.Fatal(err)
			}
			*now = now.Add(2 * time.Second)
			second, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
			if err != nil {
				t.Fatal(err)
			}
			id := first.ID
			if dir == "audit" {
				id = first.RequestID
			}
			restore := failDocumentRead(s.store, id, syscall.EIO)
			defer restore()
			for i := 0; i < 2; i++ {
				list, err := s.List(context.Background(), g.Token)
				if err != nil {
					t.Fatalf("single unreadable document disabled List: %v", err)
				}
				raw, err := json.Marshal(list)
				if err != nil {
					t.Fatal(err)
				}
				var response struct {
					Records        []Record `json:"records"`
					ReadErrorCount uint64   `json:"readErrorCount"`
				}
				if err := json.Unmarshal(raw, &response); err != nil || response.ReadErrorCount == 0 {
					t.Fatalf("partial List lacks observable read count: %s %v", raw, err)
				}
				found := false
				for _, row := range response.Records {
					found = found || row.ID == second.ID
				}
				if !found {
					t.Fatal("List lost healthy record")
				}
				log, err := s.Audits(context.Background(), g.Token)
				if err != nil || log.ReadErrorCount == 0 {
					t.Fatalf("partial audit unavailable or unobservable: %+v %v", log, err)
				}
			}
		})
	}
}

func TestReadSnapshotTemporaryFailureIsRetryable(t *testing.T) {
	s, _, _, _, dir := fixture(t)
	g := grant(t, s)
	record, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	if err != nil {
		t.Fatal(err)
	}
	restore := failDocumentRead(s.store, record.ID, syscall.EIO)
	_, err = s.Read(context.Background(), g.Token, record.ID)
	var typed *Error
	if !errors.As(err, &typed) || typed.Status != 503 || typed.Code != "APP_DIAGNOSTICS_READ_UNAVAILABLE" {
		t.Errorf("temporary read failure not retryable: %v", err)
	}
	var summary Audit
	if err := s.store.read("audit", retentionID, &summary); err != nil || summary.ReadErrorCount != 1 {
		t.Errorf("direct read error not counted: %+v %v", summary, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "diagnostics", "snapshots", record.ID+".json")); err != nil {
		t.Fatalf("temporary read removed snapshot: %v", err)
	}
	restore()
	if _, err := s.Read(context.Background(), g.Token, record.ID); err != nil {
		t.Fatalf("read did not recover: %v", err)
	}
}
