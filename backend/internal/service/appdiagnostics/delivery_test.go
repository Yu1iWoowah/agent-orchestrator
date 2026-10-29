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
				if strings.HasPrefix(api.name, "capture-") {
					docs, err := documents[Audit](s.store, "audit", nil)
					if err != nil {
						t.Fatal(err)
					}
					completed, failed := 0, 0
					for _, doc := range docs {
						if doc.value.GrantID == g.ID {
							if doc.value.Outcome == "completed" {
								completed++
							}
							if doc.value.Outcome == "failed" {
								failed++
							}
						}
					}
					if completed != 1 || failed != 1 {
						t.Fatalf("expired capture audit claims success: completed=%d failed=%d", completed, failed)
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
