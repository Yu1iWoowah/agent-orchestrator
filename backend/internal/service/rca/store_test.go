package rca

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/components/hcg/policy"
	rcapolicy "github.com/aoagents/agent-orchestrator/components/hcg/rca"
)

func testDiskStore(t *testing.T) *DiskStore {
	t.Helper()
	s, err := NewDiskStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.space = func(*os.Root) (volumeSpace, error) {
		return volumeSpace{Total: 100 << 30, Available: 10 << 30, Identity: "fixture"}, nil
	}
	return s
}

func TestDiskStoreManifestNumbersExpiryAndLatchedFailure(t *testing.T) {
	s := testDiskStore(t)
	ctx := t.Context()
	key := rcapolicy.Key{ProjectID: "agent-orchestrator", SessionID: "selected"}
	m := policy.Revise(nil, "worker", policy.Inputs{})
	if err := s.Save(ctx, key, m); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Load(ctx, key); err != nil || !ok || got.Revision != m.Revision {
		t.Fatal("manifest round trip")
	}
	state := domain.RCARecordedState{ProjectID: "agent-orchestrator", SessionID: "selected", ObservedAt: time.Now().UTC(), Status: "ready", Snapshots: []domain.RCARecordedSnapshot{}}
	if err := s.SaveSnapshot(ctx, state); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LoadSnapshot(ctx, "agent-orchestrator", "selected"); err != nil || !ok {
		t.Fatal("numeric round trip")
	}
	s.now = func() time.Time { return state.ObservedAt.Add(7 * 24 * time.Hour) }
	if _, ok, err := s.LoadSnapshot(ctx, "agent-orchestrator", "selected"); err != nil || ok {
		t.Fatal("expired projection served")
	}
	s.space = func(*os.Root) (volumeSpace, error) {
		return volumeSpace{Total: 100 << 30, Available: 2 << 30, Identity: "fixture"}, nil
	}
	if err := s.Save(ctx, key, m); err == nil || s.Reason() != "storage_low_space" {
		t.Fatal("planned write crossed T")
	}
	s.space = func(*os.Root) (volumeSpace, error) {
		return volumeSpace{Total: 100 << 30, Available: 10 << 30, Identity: "fixture"}, nil
	}
	if s.Save(ctx, key, m) == nil {
		t.Fatal("writer silently resumed")
	}
	if s.Resume(ctx) != nil || s.Save(ctx, key, m) != nil {
		t.Fatal("explicit healthy resume failed")
	}
}

func TestDiskStoreAtomicFailuresDoNotRetry(t *testing.T) {
	for _, stage := range []string{"write", "flush", "replace", "cleanup"} {
		t.Run(stage, func(t *testing.T) {
			s := testDiskStore(t)
			calls := 0
			failure := errors.New("synthetic I/O failure")
			switch stage {
			case "write":
				s.write = func(*os.File, []byte) (int, error) { calls++; return 0, failure }
			case "flush":
				s.flush = func(*os.File) error { calls++; return failure }
			case "replace":
				s.replace = func(*os.Root, string, string) error { calls++; return failure }
			case "cleanup":
				s.write = func(*os.File, []byte) (int, error) { calls++; return 0, failure }
				s.remove = func(*os.Root, string) error { return failure }
			}
			k := rcapolicy.Key{ProjectID: "agent-orchestrator", SessionID: "selected"}
			m := policy.Revise(nil, "worker", policy.Inputs{})
			if s.Save(t.Context(), k, m) == nil || s.Reason() != "storage_write_failed" {
				t.Fatal("write failure was accepted")
			}
			before := calls
			if s.Save(t.Context(), k, m) == nil || calls != before {
				t.Fatal("latched writer retried")
			}
			if stage != "cleanup" {
				if _, err := s.dir.Stat("state.next"); !os.IsNotExist(err) {
					t.Fatal("staging file retained")
				}
			}
		})
	}
}

func TestDiskStoreQuotasCountAtomicPeak(t *testing.T) {
	s := testDiskStore(t)
	s.maxHost = storageMetadataBytes + 200
	if s.Save(t.Context(), rcapolicy.Key{ProjectID: "agent-orchestrator", SessionID: "selected"}, policy.Revise(nil, "worker", policy.Inputs{})) == nil || s.Reason() != "host_limit" {
		t.Fatal("host peak ignored")
	}
	s = testDiskStore(t)
	s.maxSession = 100
	if s.Save(t.Context(), rcapolicy.Key{ProjectID: "agent-orchestrator", SessionID: "selected"}, policy.Revise(nil, "worker", policy.Inputs{})) == nil || s.Reason() != "session_limit" {
		t.Fatal("session metadata quota ignored")
	}
}

func TestDiskStoreGlobalQuotaAndConcurrentAdmission(t *testing.T) {
	s := testDiskStore(t)
	s.maxHost = storageMetadataBytes + 2048
	var wg sync.WaitGroup
	for n := range 16 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = s.Save(t.Context(), rcapolicy.Key{ProjectID: fmt.Sprintf("project-%d", n), SessionID: "selected"}, policy.Revise(nil, "worker", policy.Inputs{}))
		}(n)
	}
	wg.Wait()
	if s.Reason() != "host_limit" {
		t.Fatal("cross-project writers bypassed global quota")
	}
	info, err := s.dir.Stat("state.json")
	if err != nil {
		t.Fatal(err)
	}
	if uint64(info.Size())+storageMetadataBytes > s.maxHost {
		t.Fatal("durable state exceeded global quota")
	}
	if _, err := s.dir.Stat("state.next"); !os.IsNotExist(err) {
		t.Fatal("concurrent writer left staging")
	}
}

func TestDiskStoreStatFailureIdentityAndExpiredCleanup(t *testing.T) {
	for _, variant := range []string{"query_failure", "invalid", "changed"} {
		t.Run(variant, func(t *testing.T) {
			s := testDiskStore(t)
			k := rcapolicy.Key{ProjectID: "agent-orchestrator", SessionID: "selected"}
			m := policy.Revise(nil, "worker", policy.Inputs{})
			if err := s.Save(t.Context(), k, m); err != nil {
				t.Fatal(err)
			}
			s.space = func(*os.Root) (volumeSpace, error) {
				switch variant {
				case "query_failure":
					return volumeSpace{}, errors.New("synthetic statfs failure")
				case "invalid":
					return volumeSpace{Total: 1, Available: 2, Identity: "fixture"}, nil
				default:
					return volumeSpace{Total: 100 << 30, Available: 10 << 30, Identity: "other-volume"}, nil
				}
			}
			if s.Save(t.Context(), k, m) == nil {
				t.Fatal("unknown/changed volume admitted")
			}
			if s.Reason() == "" {
				t.Fatal("missing fixed storage stop reason")
			}
		})
	}
	s := testDiskStore(t)
	now := time.Now().UTC()
	if err := s.SaveSnapshot(t.Context(), domain.RCARecordedState{ProjectID: "agent-orchestrator", SessionID: "selected", ObservedAt: now, Status: "ready", Snapshots: []domain.RCARecordedSnapshot{}}); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now.Add(7 * 24 * time.Hour) }
	writes := 0
	s.write = func(*os.File, []byte) (int, error) { writes++; return 0, syscall.ENOSPC }
	if _, ok, err := s.LoadSnapshot(t.Context(), "agent-orchestrator", "selected"); ok || err == nil {
		t.Fatal("expired cleanup failure was accepted")
	}
	before := writes
	if _, ok, _ := s.LoadSnapshot(t.Context(), "agent-orchestrator", "selected"); ok || writes != before {
		t.Fatal("expired data served or cleanup retried")
	}
}
