package rca

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

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

func TestCompositionStorageFailureControlMatrix(t *testing.T) {
	for _, fault := range []string{"enospc", "io", "flush", "replace", "low_space"} {
		t.Run(fault, func(t *testing.T) {
			svc, _, disk, events := flowSetup(t)
			reason := "storage_write_failed"
			switch fault {
			case "enospc":
				disk.write = func(*os.File, []byte) (int, error) { return 0, syscall.ENOSPC }
			case "io":
				disk.write = func(*os.File, []byte) (int, error) { return 0, errors.New("A_SYNTHETIC_ERROR B_CONTENT_ERROR") }
			case "flush":
				disk.flush = func(*os.File) error { return syscall.EIO }
			case "replace":
				disk.replace = func(*os.Root, string, string) error { return syscall.EIO }
			case "low_space":
				reason = "storage_low_space"
				disk.space = func(*os.Root) (volumeSpace, error) {
					return volumeSpace{Total: 100 << 30, Available: 2 << 30, Identity: "fixture"}, nil
				}
			}
			if _, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Analyze: true, Limit: 200}); err != nil {
				t.Fatal(err)
			}
			out := completedFlow(t, svc)
			if out.Status != "unavailable" || out.Reason != reason || len(out.Canonical.Events) != 1 {
				t.Fatalf("failure isolation: %s %s", out.Status, out.Reason)
			}
			if _, err := events.Events(t.Context(), "selected", 0, nil, 200); err != nil {
				t.Fatal("canonical control failed")
			}
			f, err := os.CreateTemp(t.TempDir(), "native-control-")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("synthetic tool/completion\nnext synthetic prompt\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Sync(); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if next, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Analyze: true, Limit: 200}); err != nil || next.Status != "unavailable" {
				t.Fatal("latched writer restarted")
			}
		})
	}
}

type canonicalFixture struct {
	page  domain.UsageEventsPage
	calls int
}

func (f *canonicalFixture) Events(context.Context, domain.SessionID, int64, *int64, int64) (domain.UsageEventsPage, error) {
	f.calls++
	return f.page, nil
}

func flowSetup(t *testing.T) (*Service, *scopeFixture, *DiskStore, *canonicalFixture) {
	t.Helper()
	metadata, resolver, _ := scopeSetup(t)
	on := true
	metadata.project = domain.ProjectRecord{ID: "agent-orchestrator", Config: domain.ProjectConfig{RCA: domain.RCAConfig{Enabled: &on}}}
	n := int64(100)
	events := &canonicalFixture{page: domain.UsageEventsPage{ThroughID: 1, Events: []domain.UsageEventView{{ID: 1, BindingID: 1, UsageSourceID: 2, InputTokens: &n, SourceEventKey: "A_SYNTHETIC_KEY", ModelID: "B_PRIVATE_MODEL", ProviderID: "private"}}}}
	disk := testDiskStore(t)
	svc := newService(t.Context(), metadata, events, disk, resolver)
	t.Cleanup(svc.Close)
	return svc, metadata, disk, events
}

func completedFlow(t *testing.T, svc *Service) domain.RCAComposition {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		if out.Status != "running" {
			return out
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("analysis did not finish")
	return domain.RCAComposition{}
}

func TestCompositionSameServiceFlowAndCanonicalSeparation(t *testing.T) {
	svc, _, _, events := flowSetup(t)
	if _, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Limit: 200, Analyze: true}); err != nil {
		t.Fatal(err)
	}
	out := completedFlow(t, svc)
	if out.Recorded == nil || len(out.Recorded.Snapshots) != 1 || out.RequestComposition != "UNKNOWN" || len(out.Canonical.Events) != 1 {
		t.Fatalf("missing closed flow: status=%s reason=%s", out.Status, out.Reason)
	}
	if measured(out.Recorded.Snapshots[0].Buckets[0].Bytes) != 6 {
		t.Fatal("missing real reader result")
	}
	before := out.Recorded.ScopeRevision
	out = completedFlow(t, svc)
	if out.Recorded.ScopeRevision != before || events.calls < 3 {
		t.Fatal("query reanalyzed instead of reusing snapshot or skipped canonical")
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{syntheticNativeID, "A_SYNTHETIC_KEY", "B_PRIVATE_MODEL", "private_tool", "規則"} {
		if strings.Contains(string(encoded), canary) {
			t.Fatal("new query exposed external source data")
		}
	}
	if events.page.Events[0].ModelID != "B_PRIVATE_MODEL" || measured(events.page.Events[0].InputTokens) != 100 {
		t.Fatal("canonical mutated")
	}
	if err := svc.Purge(t.Context(), "selected"); err != nil {
		t.Fatal(err)
	}
	out = completedFlow(t, svc)
	if out.Recorded != nil {
		t.Fatal("purge retained numeric scope")
	}
}

func TestCompositionOffAndStorageFaultLeaveCanonicalCallerHealthy(t *testing.T) {
	svc, metadata, disk, events := flowSetup(t)
	off := false
	metadata.project.Config.RCA.Enabled = &off
	if out, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Analyze: true, Limit: 200}); err != nil || out.Reason != "disabled" {
		t.Fatal("off admitted source")
	}
	if _, err := disk.base.Stat("rca"); !os.IsNotExist(err) {
		t.Fatal("off wrote metadata")
	}
	on := true
	metadata.project.Config.RCA.Enabled = &on
	svc.ProjectPolicyChanged("agent-orchestrator", metadata.project.Config.RCAPolicySnapshot())
	disk.write = func(*os.File, []byte) (int, error) { return 0, errors.New("A_SYNTHETIC_FAILURE B_CONTENT_FAILURE") }
	if _, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Analyze: true, Limit: 200}); err != nil {
		t.Fatal(err)
	}
	out := completedFlow(t, svc)
	if out.Status != "unavailable" || out.Reason != "storage_write_failed" || len(out.Canonical.Events) != 1 {
		t.Fatalf("failure not isolated: %s %s", out.Status, out.Reason)
	}
	if _, err := events.Events(t.Context(), "selected", 0, nil, 200); err != nil {
		t.Fatal("canonical caller affected")
	}
	// The native control writer is independent: no model/provider is started.
	f, err := os.CreateTemp(t.TempDir(), "native-control-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("synthetic prompt/tool/completion\nnext prompt\n"); err != nil {
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestCompositionBackgroundPanicDoesNotExposeSourceOrStopCanonical(t *testing.T) {
	svc, _, disk, events := flowSetup(t)
	disk.write = func(*os.File, []byte) (int, error) { panic("A_SYNTHETIC_PANIC B_CONTENT_PANIC") }
	if _, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Analyze: true, Limit: 200}); err != nil {
		t.Fatal(err)
	}
	out := completedFlow(t, svc)
	if out.Status != "unavailable" || out.Reason != "analysis_failed" || len(out.Canonical.Events) != 1 {
		t.Fatalf("panic escaped RCA: %s %s", out.Status, out.Reason)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SYNTHETIC_PANIC") || strings.Contains(string(encoded), "CONTENT_PANIC") {
		t.Fatal("panic reflected")
	}
	if _, err := events.Events(t.Context(), "selected", 0, nil, 200); err != nil {
		t.Fatal("canonical affected")
	}
	if next, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Analyze: true, Limit: 200}); err != nil || next.Reason != "analysis_failed" {
		t.Fatal("background panic did not latch RCA off")
	}
}

func TestCompositionRejectsInvalidSnapshotReads(t *testing.T) {
	for _, fault := range []string{"unknown_field", "trailing", "version", "reason_canary", "expired", "expired_corrupt"} {
		t.Run(fault, func(t *testing.T) {
			svc, _, disk, _ := flowSetup(t)
			now := time.Now().UTC()
			state := domain.RCARecordedState{ProjectID: "agent-orchestrator", SessionID: "selected", ObservedAt: now, Status: "ready", Snapshots: []domain.RCARecordedSnapshot{}}
			if err := disk.SaveSnapshot(t.Context(), state); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(disk.path, "rca", "state.json")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "unknown_field", "expired_corrupt":
				b = append(b[:len(b)-1], []byte(`,"unexpected":true}`)...)
			case "trailing":
				b = append(b, []byte(` {"unexpected":true}`)...)
			case "version":
				b = []byte(strings.Replace(string(b), `"version":1`, `"version":2`, 1))
			case "reason_canary":
				b = []byte(strings.Replace(string(b), `"reason":""`, `"reason":"A_FAKE_CREDENTIAL_B_CONTENT_CANARY"`, 1))
				b = append(b[:len(b)-1], []byte(`,"unexpected":true}`)...)
			}
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(fault, "expired") {
				disk.now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
			}
			row, found, readErr := disk.LoadSnapshot(t.Context(), "agent-orchestrator", "selected")
			if found || !row.ObservedAt.IsZero() || len(row.Snapshots) != 0 || row.Reason != "" {
				t.Fatal("invalid or expired snapshot returned")
			}
			if fault != "expired" && readErr == nil {
				t.Fatal("corruption accepted")
			}
			out, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Limit: 200})
			if err != nil {
				t.Fatal(err)
			}
			if out.Recorded != nil || out.Status != "unavailable" || len(out.Canonical.Events) != 1 {
				t.Fatal("invalid snapshot escaped service")
			}
			b, err = json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "CANARY") {
				t.Fatal("unvalidated reason escaped")
			}
		})
	}
}

func TestNewEnforcesExactRootTuple(t *testing.T) {
	for _, change := range []string{"exact", "duplicate", "reordered", "device_to_managed", "absent_slot_replaced"} {
		t.Run(change, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			roots := usagesvc.SourceRoots{
				AccountCodexSessions: filepath.Join(home, ".ao", "data", "agent-accounts", "history", "codex", "sessions"),
				AccountCodexArchived: filepath.Join(home, ".ao", "data", "agent-accounts", "history", "codex", "archived_sessions"),
				CodexSessions:        filepath.Join(home, ".codex", "sessions"), CodexArchived: filepath.Join(home, ".codex", "archived_sessions"),
			}
			if err := os.MkdirAll(roots.AccountCodexSessions, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(roots.AccountCodexSessions, "rollout-lab-"+syntheticNativeID+".jsonl"), []byte(header()), 0600); err != nil {
				t.Fatal(err)
			}
			metadata, _, _ := scopeSetup(t)
			on := true
			metadata.project = domain.ProjectRecord{ID: "agent-orchestrator", Config: domain.ProjectConfig{RCA: domain.RCAConfig{Enabled: &on}}}
			switch change {
			case "duplicate":
				roots.AccountCodexArchived = roots.AccountCodexSessions
			case "reordered":
				roots.AccountCodexSessions, roots.AccountCodexArchived = roots.AccountCodexArchived, roots.AccountCodexSessions
			case "device_to_managed":
				roots.CodexSessions, roots.CodexArchived = roots.AccountCodexSessions, roots.AccountCodexArchived
			case "absent_slot_replaced":
				roots.CodexArchived = roots.CodexSessions
			}
			svc := New(t.Context(), t.TempDir(), metadata, &canonicalFixture{}, roots)
			defer svc.Close()
			if _, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Analyze: true, Limit: 200}); err != nil {
				t.Fatal(err)
			}
			out := completedFlow(t, svc)
			if change == "exact" {
				if out.Recorded == nil || len(out.Recorded.Snapshots) != 1 {
					t.Fatal("exact tuple with missing optional roots rejected")
				}
			} else if out.Reason != "scope_root_mismatch" || out.Recorded != nil {
				t.Fatalf("invalid tuple admitted: %s", out.Reason)
			}
		})
	}
}

// This store returns a partially populated row with an error, independently of
// DiskStore's own fail-closed decoder, at either service cache load boundary.
type partialSnapshotStore struct {
	ProjectionStore
	calls, failAt int
}

func (s *partialSnapshotStore) LoadSnapshot(ctx context.Context, p domain.ProjectID, id domain.SessionID) (domain.RCARecordedState, bool, error) {
	s.calls++
	if s.calls == s.failAt {
		return domain.RCARecordedState{Reason: "A_FAKE_CREDENTIAL_B_CONTENT_CANARY"}, true, errors.New("synthetic read error")
	}
	return s.ProjectionStore.LoadSnapshot(ctx, p, id)
}
func TestCompositionRejectsPartialStoreRowsAtBothLoads(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			svc, _, disk, _ := flowSetup(t)
			svc.projection = &partialSnapshotStore{ProjectionStore: disk, failAt: failAt}
			svc.jobs["selected"] = jobState{revision: "new", status: "ready"}
			out, err := svc.Get(t.Context(), "selected", domain.RCAQuery{Limit: 200})
			if err != nil || out.Recorded != nil || out.Status != "unavailable" || out.Reason != "storage_read_failed" || len(out.Canonical.Events) != 1 {
				t.Fatal("partial store row escaped cache load")
			}
		})
	}
}
