package appdiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestDeliveryRechecksExpiryAfterIO(t *testing.T) {
	apis := []struct {
		name  string
		read  string
		write string
		run   func(*Service, Credential, Record) (any, error)
	}{
		{"read-snapshot", "snapshot", "", func(s *Service, g Credential, r Record) (any, error) {
			return s.Read(context.Background(), g.Token, r.ID)
		}},
		{"read-audit", "", "read", func(s *Service, g Credential, r Record) (any, error) {
			return s.Read(context.Background(), g.Token, r.ID)
		}},
		{"capture-snapshot", "", "snapshot", func(s *Service, g Credential, _ Record) (any, error) {
			return s.Capture(context.Background(), g.Token, CaptureRequest{})
		}},
		{"capture-audit", "", "completed", func(s *Service, g Credential, _ Record) (any, error) {
			return s.Capture(context.Background(), g.Token, CaptureRequest{})
		}},
		{"capture-prune", "snapshot", "", func(s *Service, g Credential, _ Record) (any, error) {
			return s.Capture(context.Background(), g.Token, CaptureRequest{})
		}},
		{"list-snapshot", "snapshot", "", func(s *Service, g Credential, _ Record) (any, error) { return s.List(context.Background(), g.Token) }},
		{"list-audit", "", "list", func(s *Service, g Credential, _ Record) (any, error) { return s.List(context.Background(), g.Token) }},
		{"audit-read", "audit", "", func(s *Service, g Credential, _ Record) (any, error) { return s.Audits(context.Background(), g.Token) }},
		{"audit-write", "", "audit_read", func(s *Service, g Credential, _ Record) (any, error) { return s.Audits(context.Background(), g.Token) }},
		{"runtime-audit-read", "audit", "", func(s *Service, g Credential, _ Record) (any, error) {
			return s.AuditRuntime(context.Background(), "epoch-1", g.ID)
		}},
		{"runtime-audit-write", "", "audit_read", func(s *Service, g Credential, _ Record) (any, error) {
			return s.AuditRuntime(context.Background(), "epoch-1", g.ID)
		}},
	}
	for _, api := range apis {
		for _, offset := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
			t.Run(api.name+"/"+offset.String(), func(t *testing.T) {
				s, _, _, now, dir := fixture(t)
				g := grant(t, s)
				seed, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
				if err != nil {
					t.Fatal(err)
				}
				*now = g.ExpiresAt.Add(-time.Millisecond)
				crossed := false
				advance := func() { crossed = true; *now = g.ExpiresAt.Add(offset) }
				read := s.store.readContent
				s.store.readContent = func(f *os.File) ([]byte, error) {
					data, err := read(f)
					if !crossed && ((api.read == "snapshot" && strings.Contains(f.Name(), seed.ID)) || (api.read == "audit" && strings.Contains(f.Name(), "audit"))) {
						advance()
					}
					return data, err
				}
				write := s.store.writeContent
				s.store.writeContent = func(f *os.File, data []byte) (int, error) {
					n, err := write(f, data)
					var audit Audit
					_ = json.Unmarshal(data, &audit)
					if !crossed && ((api.write == "snapshot" && strings.Contains(f.Name(), "snapshots")) || (api.write != "" && api.write == audit.Outcome)) {
						advance()
					}
					return n, err
				}
				value, err := api.run(s, g, seed)
				if !crossed {
					t.Fatal("I/O boundary was not exercised")
				}
				if offset < 0 {
					if err != nil || reflect.ValueOf(value).IsZero() {
						t.Fatalf("unexpired result rejected: %v", err)
					}
					return
				}
				var typed *Error
				if !errors.As(err, &typed) || typed.Code != "APP_DIAGNOSTICS_GRANT_EXPIRED" || !reflect.ValueOf(value).IsZero() {
					t.Fatalf("expired content delivered: zero=%t error=%v", reflect.ValueOf(value).IsZero(), err)
				}
				files, err := os.ReadDir(filepath.Join(dir, "diagnostics", "snapshots"))
				if err != nil || len(files) != 0 {
					t.Fatalf("expired snapshots retained: %d %v", len(files), err)
				}
				{
					docs, err := documents[Audit](s.store, "audit", nil)
					if err != nil {
						t.Fatal(err)
					}
					completed, failed, undelivered := 0, 0, 0
					for _, doc := range docs {
						if doc.value.GrantID == g.ID {
							if doc.value.Outcome == "completed" {
								completed++
							}
							if doc.value.Outcome == "failed" {
								failed++
								if doc.value.ReasonCode != "APP_DIAGNOSTICS_GRANT_EXPIRED" {
									t.Errorf("failed delivery reason missing: %s", doc.value.ReasonCode)
								}
							}
							if doc.value.Outcome == "list" || doc.value.Outcome == "audit_read" || doc.value.Outcome == "read" {
								undelivered++
							}
						}
					}
					if completed != 1 || failed != 1 || undelivered != 0 {
						t.Fatalf("expired delivery audit claims success: completed=%d failed=%d undelivered=%d", completed, failed, undelivered)
					}
				}
			})
		}
	}
}

func TestDeliveryRejectsCancellationDuringIOWithoutRevokingGrant(t *testing.T) {
	for _, method := range []string{"read", "capture"} {
		t.Run(method, func(t *testing.T) {
			s, _, _, _, dir := fixture(t)
			g := grant(t, s)
			seed, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if method == "read" {
				original := s.store.readContent
				s.store.readContent = func(f *os.File) ([]byte, error) {
					data, err := original(f)
					if strings.Contains(f.Name(), seed.ID) {
						cancel()
					}
					return data, err
				}
			} else {
				original := s.store.writeContent
				s.store.writeContent = func(f *os.File, b []byte) (int, error) {
					n, err := original(f, b)
					if strings.Contains(f.Name(), "snapshots") {
						cancel()
					}
					return n, err
				}
				// Avoid rate limiting the second capture; cancellation concerns I/O only.
				s.lastCapture = time.Time{}
			}
			var result Record
			if method == "read" {
				result, err = s.Read(ctx, g.Token, seed.ID)
			} else {
				result, err = s.Capture(ctx, g.Token, CaptureRequest{})
			}
			if err == nil || result.ID != "" || result.Snapshot.InstanceID != "" {
				t.Fatalf("cancelled content delivered: %v", err)
			}
			if _, err := s.Status(context.Background(), g.Token); err != nil {
				t.Fatalf("cancellation revoked grant: %v", err)
			}
			files, err := os.ReadDir(filepath.Join(dir, "diagnostics", "snapshots"))
			if err != nil || len(files) != 1 {
				t.Fatalf("cancelled capture retained: %d %v", len(files), err)
			}
		})
	}
}

type deliverySessionReader struct {
	sessionReader
	after func()
}

func (r deliverySessionReader) Get(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	row, err := r.sessionReader.Get(ctx, id)
	r.after()
	return row, err
}

func TestAuthorizationQueryCannotCrossExpiry(t *testing.T) {
	s, rows, _, now, _ := fixture(t)
	g := grant(t, s)
	*now = g.ExpiresAt.Add(-time.Millisecond)
	s.sessions = deliverySessionReader{rows, func() { *now = g.ExpiresAt.Add(time.Millisecond) }}
	value, err := s.Status(context.Background(), g.Token)
	if err == nil || !reflect.ValueOf(value).IsZero() {
		t.Fatalf("authorization query crossed expiry and returned content: %v", err)
	}
}

func TestDeliveryRejectsRuntimeReplyAfterConcurrentRevocation(t *testing.T) {
	s, _, rt, _, dir := fixture(t)
	g := grant(t, s)
	entered, release := make(chan struct{}), make(chan struct{})
	original := rt.capture
	rt.capture = func(ctx context.Context, in CaptureRequest) (Snapshot, error) {
		close(entered)
		<-release
		return original(ctx, in)
	}
	type result struct {
		record Record
		err    error
	}
	done := make(chan result, 1)
	go func() { r, err := s.Capture(context.Background(), g.Token, CaptureRequest{}); done <- result{r, err} }()
	<-entered
	err := s.RevokeRuntime(context.Background(), "epoch-1", g.ID)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	out := <-done
	if out.err == nil || out.record.ID != "" {
		t.Fatalf("revoked capture delivered: %v", out.err)
	}
	files, err := os.ReadDir(filepath.Join(dir, "diagnostics", "snapshots"))
	if err != nil || len(files) != 0 {
		t.Fatalf("revoked capture retained: %v", err)
	}
}

func TestReadCrossingExpiryWhileRevocationWaitsForIO(t *testing.T) {
	s, _, _, _, _ := fixture(t)
	g := grant(t, s)
	record, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(g.ExpiresAt.Add(-time.Millisecond).UnixNano())
	s.now = func() time.Time { return time.Unix(0, clock.Load()) }
	entered, release := make(chan struct{}), make(chan struct{})
	original := s.store.readContent
	var once sync.Once
	s.store.readContent = func(f *os.File) ([]byte, error) {
		data, err := original(f)
		if strings.Contains(f.Name(), record.ID) {
			once.Do(func() { close(entered); <-release })
		}
		return data, err
	}
	type result struct {
		record Record
		err    error
	}
	readDone := make(chan result, 1)
	go func() { r, err := s.Read(context.Background(), g.Token, record.ID); readDone <- result{r, err} }()
	<-entered
	revoking, revoked := make(chan struct{}), make(chan error, 1)
	go func() { close(revoking); revoked <- s.RevokeRuntime(context.Background(), "epoch-1", g.ID) }()
	<-revoking
	clock.Store(g.ExpiresAt.Add(time.Millisecond).UnixNano())
	close(release)
	out := <-readDone
	<-revoked
	if out.err == nil || out.record.ID != "" {
		t.Fatalf("expired read delivered while revoke waited: %v", out.err)
	}
	if _, err := s.Status(context.Background(), g.Token); err == nil {
		t.Fatal("grant remained usable")
	}
}

func TestAuditDeliveryCancellationRecordsFailure(t *testing.T) {
	for _, method := range []string{"list", "audit", "runtime-audit"} {
		t.Run(method, func(t *testing.T) {
			s, _, _, _, _ := fixture(t)
			g := grant(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			write := s.store.writeContent
			var requestID string
			s.store.writeContent = func(f *os.File, data []byte) (int, error) {
				n, err := write(f, data)
				var audit Audit
				if json.Unmarshal(data, &audit) == nil && (audit.Outcome == "list" || audit.Outcome == "audit_read") {
					requestID = audit.RequestID
					cancel()
				}
				return n, err
			}
			var err error
			var result any
			switch method {
			case "list":
				result, err = s.List(ctx, g.Token)
			case "audit":
				result, err = s.Audits(ctx, g.Token)
			case "runtime-audit":
				result, err = s.AuditRuntime(ctx, "epoch-1", g.ID)
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != "APP_SNAPSHOT_TIMEOUT" || !reflect.ValueOf(result).IsZero() {
				t.Fatalf("cancelled response delivered: %v", err)
			}
			if requestID == "" {
				t.Fatal("audit I/O boundary not exercised")
			}
			var audit Audit
			if err := s.store.read("audit", requestID, &audit); err != nil || audit.Outcome != "failed" || audit.ReasonCode != "APP_SNAPSHOT_TIMEOUT" {
				t.Fatalf("undelivered response audit must be failed: %+v %v", audit, err)
			}
			if _, err := s.Status(context.Background(), g.Token); err != nil {
				t.Fatalf("cancellation revoked grant: %v", err)
			}
		})
	}
}

func TestFailedCaptureCleanupBlocksDeliveryAndRetries(t *testing.T) {
	s, _, rt, now, dir := fixture(t)
	g := grant(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	write := s.store.writeContent
	var requestID string
	s.store.writeContent = func(f *os.File, data []byte) (int, error) {
		n, err := write(f, data)
		var audit Audit
		if json.Unmarshal(data, &audit) == nil && audit.Outcome == "completed" {
			requestID = audit.RequestID
			cancel()
		}
		return n, err
	}
	remove := s.store.removeFile
	s.store.removeFile = func(name string) error {
		if filepath.Dir(name) == "snapshots" {
			return syscall.EACCES
		}
		return remove(name)
	}
	record, err := s.Capture(ctx, g.Token, CaptureRequest{})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "APP_DIAGNOSTICS_CLEANUP_FAILED" || !reflect.ValueOf(record).IsZero() {
		t.Errorf("failed capture cleanup not observable: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "diagnostics", "snapshots"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed unlink fixture not retained: %v %v", entries, err)
	}
	id := strings.TrimSuffix(entries[0].Name(), ".json")
	read, err := s.Read(context.Background(), g.Token, id)
	if err == nil || !reflect.ValueOf(read).IsZero() {
		t.Errorf("failed capture readable with same grant: %v", err)
	}
	list, err := s.List(context.Background(), g.Token)
	if len(list.Records) != 0 {
		t.Errorf("failed capture listed with same grant: %v", err)
	}
	var audit Audit
	if err := s.store.read("audit", requestID, &audit); err != nil || audit.Outcome != "failed" || audit.ReasonCode != "APP_SNAPSHOT_TIMEOUT" {
		t.Errorf("cleanup failure not audited: %+v %v", audit, err)
	}
	if _, err := s.Status(context.Background(), g.Token); err != nil {
		t.Fatalf("cleanup failure revoked grant: %v", err)
	}
	*now = now.Add(3 * time.Second)
	called := false
	rt.capture = func(context.Context, CaptureRequest) (Snapshot, error) {
		called = true
		return Snapshot{}, nil
	}
	_, err = s.Capture(context.Background(), g.Token, CaptureRequest{})
	if called || !errors.As(err, &typed) || typed.Code != "APP_DIAGNOSTICS_CLEANUP_FAILED" {
		t.Errorf("new capture allowed to accumulate failed files: called=%t %v", called, err)
	}
	s.store.removeFile = remove
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "diagnostics", "snapshots", id+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed snapshot deletion not retried: %v", err)
	}
	list, err = s.List(context.Background(), g.Token)
	if err != nil || len(list.Records) != 0 {
		t.Fatalf("list did not recover after cleanup: %+v %v", list, err)
	}
	if _, err := s.Status(context.Background(), g.Token); err != nil {
		t.Fatalf("cleanup retry revoked grant: %v", err)
	}
}

func TestDiscardedSnapshotsRemainFencedAcrossRevokeAndRestart(t *testing.T) {
	for _, recovery := range []string{"revoke", "restart"} {
		t.Run(recovery, func(t *testing.T) {
			s, rows, rt, now, dir := fixture(t)
			g := grant(t, s)
			record, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
			if err != nil {
				t.Fatal(err)
			}
			remove := s.store.removeFile
			s.store.removeFile = func(name string) error {
				if filepath.Dir(name) == "snapshots" {
					return syscall.EACCES
				}
				return remove(name)
			}
			if err := s.store.discardSnapshot(record.ID, g.ID); !errors.Is(err, syscall.EACCES) {
				t.Fatalf("unlink failure not exercised: %v", err)
			}
			if recovery == "revoke" {
				other := grant(t, s)
				out, err := s.RevokeID(context.Background(), other.Token, other.ID)
				if err != nil || out.Cleanup != "complete" {
					t.Fatalf("other grant cleanup affected: %+v %v", out, err)
				}
				out, err = s.RevokeID(context.Background(), g.Token, g.ID)
				if err != nil || !out.Revoked || out.Cleanup != "failed" {
					t.Fatalf("discarded snapshot omitted from revoke result: %+v %v", out, err)
				}
				stale := Audit{RequestID: record.RequestID, GrantID: g.ID, At: now.Add(-8 * 24 * time.Hour), Outcome: "failed"}
				if err := s.store.audit(stale); err != nil {
					t.Fatal(err)
				}
				if err := s.Sweep(context.Background()); err == nil {
					t.Fatal("pending cleanup must remain observable")
				}
				if _, err := os.Stat(filepath.Join(dir, "diagnostics", "audit", stale.RequestID+".json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("blocked snapshot skipped healthy audit retention: %v", err)
				}
				s.store.removeFile = remove
				if err := s.Sweep(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				var typed *Error
				if err := s.Close(); !errors.As(err, &typed) || typed.Code != "APP_DIAGNOSTICS_CLEANUP_FAILED" {
					t.Fatalf("shutdown cleanup failure not reported: %v", err)
				}
				next, err := New(dir, rows, rt, func() time.Time { return *now })
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = next.Close() })
				if _, err := next.Read(context.Background(), g.Token, record.ID); err == nil {
					t.Fatal("restart revived rejected capture")
				}
				fresh := grant(t, next)
				out, err := next.List(context.Background(), fresh.Token)
				if err != nil || len(out.Records) != 0 {
					t.Fatalf("restarted service published discarded snapshot: %+v %v", out, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "diagnostics", "snapshots", record.ID+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("discarded snapshot not cleaned after %s: %v", recovery, err)
			}
		})
	}
}
