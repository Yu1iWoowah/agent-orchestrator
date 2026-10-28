package appdiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

type sessions struct {
	mu  sync.Mutex
	row domain.Session
	err error
}

func (s *sessions) Get(ctx context.Context, _ domain.SessionID) (domain.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.Session{}, err
	}
	return s.row, s.err
}

type runtime struct {
	state   RuntimeState
	capture func(context.Context, CaptureRequest) (Snapshot, error)
}

func (r *runtime) State() RuntimeState { return r.state }
func (r *runtime) Capture(ctx context.Context, in CaptureRequest) (Snapshot, error) {
	return r.capture(ctx, in)
}
func fixture(t *testing.T) (*Service, *sessions, *runtime, *time.Time, string) {
	t.Helper()
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	rows := &sessions{row: domain.Session{SessionRecord: domain.SessionRecord{ID: "orchestrator-1", ProjectID: "project", Kind: domain.KindOrchestrator, Metadata: domain.SessionMetadata{ControllerGeneration: "launch-1"}}}}
	rt := &runtime{state: RuntimeState{InstanceID: "app-1", Epoch: "epoch-1", Ready: true, Windows: []Window{{ID: "window-1", Generation: 1}}}}
	rt.capture = func(_ context.Context, in CaptureRequest) (Snapshot, error) {
		return Snapshot{SchemaVersion: 1, InstanceID: "app-1", WindowID: in.WindowID, Generation: 1, CapturedAt: now, Completeness: "partial"}, nil
	}
	dir := t.TempDir()
	svc, err := New(dir, rows, rt, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc, rows, rt, &now, dir
}
func grant(t *testing.T, s *Service) Credential {
	t.Helper()
	g, err := s.issueGrant(context.Background(), "orchestrator-1", "epoch-1")
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func TestGrantRequiresReadyRuntimeAndLiveOrchestrator(t *testing.T) {
	s, rows, rt, _, _ := fixture(t)
	if _, err := s.Status(context.Background(), "missing"); err == nil {
		t.Fatal("default service authorized an ungranted caller")
	}
	rt.state.Ready = false
	if _, err := s.issueGrant(context.Background(), "orchestrator-1", "epoch-1"); err == nil {
		t.Fatal("granted without collector")
	}
	rt.state.Ready = true
	rows.row.Kind = domain.KindWorker
	if _, err := s.issueGrant(context.Background(), "orchestrator-1", "epoch-1"); err == nil {
		t.Fatal("worker was granted app access")
	}
	rows.row.Kind = domain.KindOrchestrator
	rows.row.IsTerminated = true
	if _, err := s.issueGrant(context.Background(), "orchestrator-1", "epoch-1"); err == nil {
		t.Fatal("terminated session was granted")
	}
}
func TestGrantFencesLaunchInstanceAndExpiry(t *testing.T) {
	for _, change := range []string{"launch", "expiry", "instance", "epoch", "terminated", "project"} {
		t.Run(change, func(t *testing.T) {
			s, rows, rt, now, _ := fixture(t)
			g := grant(t, s)
			if _, err := s.Status(context.Background(), g.Token); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "project":
				rows.row.ProjectID = "other-project"
			case "launch":
				rows.row.Metadata.ControllerGeneration = "launch-2"
			case "expiry":
				*now = now.Add(15 * time.Minute)
			case "instance":
				rt.state.InstanceID = "app-2"
			case "epoch":
				rt.state.Epoch = "epoch-2"
			case "terminated":
				rows.row.IsTerminated = true
			}
			if _, err := s.Status(context.Background(), g.Token); err == nil {
				t.Fatal("stale grant still authorized")
			}
		})
	}
}
func TestSnapshotOwnedByGrantAndRevokeRemovesCredentialAndPayload(t *testing.T) {
	s, _, _, _, dir := fixture(t)
	g := grant(t, s)
	out, err := s.Capture(context.Background(), g.Token, CaptureRequest{WindowID: "window-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(context.Background(), g.Token, out.ID); err != nil {
		t.Fatal(err)
	}
	second := grant(t, s)
	if _, err := s.Read(context.Background(), second.Token, out.ID); err == nil {
		t.Fatal("another grant read snapshot")
	}
	if err := s.Revoke(context.Background(), g.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(context.Background(), g.Token, out.ID); err == nil {
		t.Fatal("revoked grant read snapshot")
	}
	for _, name := range []string{filepath.Join("grants", g.ID+".json"), filepath.Join("snapshots", out.ID+".json")} {
		if _, err := os.Stat(filepath.Join(dir, "diagnostics", name)); !os.IsNotExist(err) {
			t.Fatalf("retained revoked file %s: %v", name, err)
		}
	}
}
func TestRevokeCancelsPendingCaptureAndRejectsLateReply(t *testing.T) {
	s, _, rt, _, dir := fixture(t)
	g := grant(t, s)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	rt.capture = func(ctx context.Context, in CaptureRequest) (Snapshot, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return Snapshot{SchemaVersion: 1, InstanceID: "app-1", WindowID: in.WindowID, Generation: 1, Completeness: "partial"}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Capture(context.Background(), g.Token, CaptureRequest{WindowID: "window-1"})
		done <- err
	}()
	<-entered
	if err := s.Revoke(context.Background(), g.Token); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("revoke did not cancel runtime")
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("late result was delivered")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "diagnostics", "snapshots"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("late payload persisted: %v %v", entries, err)
	}
}
func TestSnapshotValidationAndSingleInflight(t *testing.T) {
	s, _, rt, _, _ := fixture(t)
	g := grant(t, s)
	rt.capture = func(_ context.Context, in CaptureRequest) (Snapshot, error) {
		return Snapshot{SchemaVersion: 1, InstanceID: "another-app", WindowID: in.WindowID, Generation: 1, Completeness: "partial"}, nil
	}
	if _, err := s.Capture(context.Background(), g.Token, CaptureRequest{WindowID: "window-1"}); err == nil {
		t.Fatal("accepted reply from different app")
	}
}
func TestStoreRejectsSymlinksAndUnsafePermissions(t *testing.T) {
	for _, kind := range []string{"symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			if goruntime.GOOS == "windows" && kind == "permissions" {
				t.Skip("POSIX mode is not an ACL; native Windows ACL test covers this boundary")
			}
			dir := t.TempDir()
			target := filepath.Join(dir, "diagnostics")
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), target); err != nil {
					t.Skip(err)
				}
			} else {
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			s, err := New(dir, nil, nil, time.Now)
			if err == nil {
				_ = s.Close()
				t.Fatal("unsafe diagnostics root accepted")
			}
		})
	}
}
func TestRestartDoesNotRestoreGrantOrUnfinishedSnapshots(t *testing.T) {
	s, rows, rt, now, dir := fixture(t)
	g := grant(t, s)
	if _, err := s.Capture(context.Background(), g.Token, CaptureRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := New(dir, rows, rt, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Status(context.Background(), g.Token); err == nil {
		t.Fatal("grant restored after restart")
	}
	files, _ := os.ReadDir(filepath.Join(dir, "diagnostics", "snapshots"))
	if len(files) != 0 {
		t.Fatal("old grant payload retained")
	}
}
func TestStorageFailureDoesNotReturnSuccess(t *testing.T) {
	s, _, _, _, dir := fixture(t)
	g := grant(t, s)
	if err := os.Rename(filepath.Join(dir, "diagnostics", "audit"), filepath.Join(dir, "diagnostics", "moved-audit")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "diagnostics", "audit"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Capture(context.Background(), g.Token, CaptureRequest{}); err == nil {
		t.Fatal("audit failure returned success")
	}
	if _, err := s.Read(context.Background(), "wrong-token", "../secret"); !errors.As(err, new(*Error)) {
		t.Fatalf("untyped boundary error: %v", err)
	}
}

func TestListsAndAuditAreGrantScoped(t *testing.T) {
	s, _, _, now, _ := fixture(t)
	first := grant(t, s)
	row, err := s.Capture(context.Background(), first.Token, CaptureRequest{})
	if err != nil {
		t.Fatal(err)
	}
	second := grant(t, s)
	list, err := s.List(context.Background(), second.Token)
	if err != nil || len(list) != 0 {
		t.Fatalf("cross-grant list: %v %v", list, err)
	}
	own, err := s.List(context.Background(), first.Token)
	if err != nil || len(own) != 1 || own[0].ID != row.ID {
		t.Fatal("own record unavailable")
	}
	audit, err := s.Audits(context.Background(), second.Token)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range audit.Records {
		if entry.GrantID != second.ID {
			t.Fatal("cross-grant audit")
		}
	}
	if err := s.RevokeID(context.Background(), second.Token, first.ID); err == nil {
		t.Fatal("wrong revoke path accepted")
	}
	*now = now.Add(2 * time.Second)
}
func TestCaptureRejectsWindowDisappearingBeforeCommit(t *testing.T) {
	s, _, rt, _, _ := fixture(t)
	g := grant(t, s)
	original := rt.capture
	rt.capture = func(ctx context.Context, in CaptureRequest) (Snapshot, error) {
		out, err := original(ctx, in)
		rt.state.Windows = []Window{{ID: "another-window", Generation: 1}}
		return out, err
	}
	if _, err := s.Capture(context.Background(), g.Token, CaptureRequest{}); err == nil {
		t.Fatal("removed window snapshot committed")
	}
}
func TestUnsupportedCollectorIsNotATimeout(t *testing.T) {
	s, _, rt, _, _ := fixture(t)
	g := grant(t, s)
	rt.capture = func(context.Context, CaptureRequest) (Snapshot, error) {
		return Snapshot{}, &Error{Code: "APP_DIAGNOSTICS_UNSUPPORTED", Status: 503}
	}
	_, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "APP_DIAGNOSTICS_UNSUPPORTED" {
		t.Fatalf("unsupported misclassified: %v", err)
	}
}
func TestCaptureSingleInflightAndNoRetry(t *testing.T) {
	s, _, rt, _, _ := fixture(t)
	g := grant(t, s)
	entered := make(chan struct{})
	release := make(chan struct{})
	original := rt.capture
	rt.capture = func(ctx context.Context, in CaptureRequest) (Snapshot, error) {
		close(entered)
		<-release
		return original(ctx, in)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Capture(context.Background(), g.Token, CaptureRequest{}); done <- err }()
	<-entered
	_, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "APP_DIAGNOSTICS_BUSY" {
		t.Fatalf("did not refuse concurrent capture: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestRetentionPrunesOldestSnapshotAndExpiresAudit(t *testing.T) {
	s, _, _, now, _ := fixture(t)
	g := grant(t, s)
	var first string
	for i := 0; i < 21; i++ {
		row, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = row.ID
		}
		*now = now.Add(2 * time.Second)
	}
	list, err := s.List(context.Background(), g.Token)
	if err != nil || len(list) != 20 {
		t.Fatalf("retention: %d %v", len(list), err)
	}
	if _, err := s.Read(context.Background(), g.Token, first); err == nil {
		t.Fatal("oldest snapshot retained over cap")
	}
	*now = now.Add(8 * 24 * time.Hour)
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := s.store.entries("snapshots")
	if err != nil || len(entries) != 0 {
		t.Fatalf("expired records retained: %v", err)
	}
}

func TestTrustedRuntimeRevokeFencesEpoch(t *testing.T) {
	s, _, _, _, _ := fixture(t)
	g := grant(t, s)
	if err := s.RevokeRuntime(context.Background(), "old-epoch", g.ID); err == nil {
		t.Fatal("old runtime revoked current grant")
	}
	if _, err := s.Status(context.Background(), g.Token); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeRuntime(context.Background(), "epoch-1", g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Status(context.Background(), g.Token); err == nil {
		t.Fatal("runtime revoke retained grant")
	}
}

func TestAuditRetentionReportsDroppedHistory(t *testing.T) {
	s, _, _, now, _ := fixture(t)
	g := grant(t, s)
	if err := s.store.write("audit", "11111111-1111-4111-8111-111111111111", Audit{RequestID: "11111111-1111-4111-8111-111111111111", GrantID: g.ID, At: now.Add(-8 * 24 * time.Hour), Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	log, err := s.Audits(context.Background(), g.Token)
	if err != nil {
		t.Fatal(err)
	}
	if log.DroppedCount != 1 || log.OldestRetainedAt == nil {
		t.Fatalf("retention was silent: %+v", log)
	}
	for _, row := range log.Records {
		if row.RequestID == "11111111-1111-4111-8111-111111111111" {
			t.Fatal("expired audit returned")
		}
	}
}

func TestRejectedCaptureAuditedWithoutToken(t *testing.T) {
	s, _, _, _, _ := fixture(t)
	const secret = "credential-must-never-be-written"
	if _, err := s.Capture(context.Background(), secret, CaptureRequest{}); err == nil {
		t.Fatal("ungranted capture succeeded")
	}
	entries, err := s.store.entries("audit")
	if err != nil || len(entries) != 1 {
		t.Fatalf("denied capture not audited: %d %v", len(entries), err)
	}
	raw, err := s.store.root.ReadFile(filepath.Join("audit", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), `"outcome":"denied"`) {
		t.Fatal("rejection audit leaked or omitted classification")
	}
}

func TestDiskFullDoesNotPublishSnapshotAndRemovesTemporaryFile(t *testing.T) {
	s, _, _, _, _ := fixture(t)
	g := grant(t, s)
	s.store.writeContent = func(file *os.File, data []byte) (int, error) {
		if filepath.Base(filepath.Dir(file.Name())) == "snapshots" {
			return 0, syscall.ENOSPC
		}
		return file.Write(data)
	}
	_, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "APP_DIAGNOSTICS_STORAGE_FAILED" {
		t.Fatalf("disk full not surfaced safely: %v", err)
	}
	entries, err := s.store.entries("snapshots")
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial or temporary snapshot retained: %v %v", entries, err)
	}
	log, err := s.Audits(context.Background(), g.Token)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range log.Records {
		if entry.Outcome == "storage_failed" {
			found = true
		}
	}
	if !found {
		t.Fatal("disk-full capture has no failure audit")
	}
}

func TestCanceledCaptureAndTransientLookupPreserveGrant(t *testing.T) {
	s, rows, rt, _, _ := fixture(t)
	g := grant(t, s)
	prior, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, lookupErr := range []error{context.Canceled, errors.New("database busy")} {
		rows.err = lookupErr
		_, err := s.Status(context.Background(), g.Token)
		var typed *Error
		if !errors.As(err, &typed) || typed.Status != 503 {
			t.Fatalf("lookup failed destructively: %v", err)
		}
		if err := s.Sweep(context.Background()); err == nil {
			t.Fatal("sweep hid lookup failure")
		}
		rows.err = nil
		if _, err := s.Read(context.Background(), g.Token, prior.ID); err != nil {
			t.Fatalf("prior snapshot lost: %v", err)
		}
	}
	s.lastCapture = time.Time{}
	ctx, cancel := context.WithCancel(context.Background())
	rt.capture = func(ctx context.Context, _ CaptureRequest) (Snapshot, error) { cancel(); return Snapshot{}, ctx.Err() }
	if _, err := s.Capture(ctx, g.Token, CaptureRequest{}); err == nil {
		t.Fatal("canceled capture succeeded")
	}
	if _, err := s.Read(context.Background(), g.Token, prior.ID); err != nil {
		t.Fatalf("cancel revoked grant: %v", err)
	}
	rows.err = apierr.NotFound("SESSION_NOT_FOUND", "missing")
	if _, err := s.Status(context.Background(), g.Token); err == nil {
		t.Fatal("missing session authorized")
	}
	if len(s.grants) != 0 {
		t.Fatal("missing session not revoked")
	}
}

func TestCapturePinsGrantProjectBeforeRuntime(t *testing.T) {
	for _, project := range []domain.ProjectID{"", "project", "other"} {
		t.Run(string(project), func(t *testing.T) {
			s, _, rt, _, _ := fixture(t)
			g := grant(t, s)
			called := false
			original := rt.capture
			rt.capture = func(ctx context.Context, in CaptureRequest) (Snapshot, error) {
				called = true
				if in.ProjectID != "project" {
					t.Errorf("runtime received project %q", in.ProjectID)
				}
				return original(ctx, in)
			}
			_, err := s.Capture(context.Background(), g.Token, CaptureRequest{ProjectID: string(project)})
			if project == "other" {
				if err == nil || called {
					t.Fatal("cross-project request forwarded")
				}
				return
			}
			if err != nil || !called {
				t.Fatalf("valid project rejected: %v", err)
			}
		})
	}
}

func TestSweepContinuesAfterRevokeStorageFailure(t *testing.T) {
	s, _, _, now, dir := fixture(t)
	bad := []Credential{grant(t, s), grant(t, s)}
	good := grant(t, s)
	// Two failing entries make an early return observable for every map order.
	for _, credential := range bad {
		file := filepath.Join(dir, "diagnostics", "grants", credential.ID+".json")
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(file, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(file, "block"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(16 * time.Minute)
	err := s.Sweep(context.Background())
	if err == nil || strings.Count(err.Error(), "APP_DIAGNOSTICS_STORAGE_FAILED") != 2 {
		t.Fatalf("cleanup errors not aggregated: %v", err)
	}
	if len(s.grants) != 0 {
		t.Fatal("sweep stopped before processing all grants")
	}
	if _, err := os.Stat(filepath.Join(dir, "diagnostics", "grants", good.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("valid credential retained after earlier failure: %v", err)
	}
}

func TestCorruptDocumentsDoNotDisableResetOrPrune(t *testing.T) {
	s, _, _, now, dir := fixture(t)
	for _, name := range []string{"foreign.txt", uuid.NewString() + ".json", uuid.NewString() + ".tmp"} {
		if err := os.WriteFile(filepath.Join(dir, "diagnostics", "audit", name), []byte(`{"futureSchema":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	g := grant(t, s)
	if err := s.store.prune(*now); err != nil {
		t.Fatalf("one bad file prevented prune: %v", err)
	}
	if _, err := s.Audits(context.Background(), g.Token); err != nil {
		t.Fatal(err)
	}
	var summary Audit
	if err := s.store.read("audit", retentionID, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.SkippedCount != 3 || summary.ReasonCode != "APP_DIAGNOSTICS_DOCUMENT_INVALID" {
		t.Fatalf("missing bounded corruption reason: %+v", summary)
	}
	raw, err := s.store.root.ReadFile(filepath.Join("audit", retentionID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "foreign.txt") {
		t.Fatal("corruption audit leaked a path")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir, nil, nil, func() time.Time { return *now })
	if err != nil {
		t.Fatalf("one bad file disabled startup: %v", err)
	}
	defer reopened.Close()
}

func TestAuditWriteDoesNotScanSnapshotsAndEnforcesCountBoundary(t *testing.T) {
	s, _, _, now, _ := fixture(t)
	// Unknown snapshot schema cannot affect an unrelated audit append.
	if err := s.store.write("snapshots", uuid.NewString(), map[string]string{"future": "schema"}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.audit(Audit{RequestID: uuid.NewString(), At: *now, Outcome: "read"}); err != nil {
		t.Fatalf("audit scanned snapshots: %v", err)
	}
	for i := 0; i < 10000; i++ {
		if err := s.store.write("audit", uuid.NewString(), Audit{At: *now, Outcome: "read"}); err != nil {
			t.Fatal(err)
		}
	}
	// Reinitialize its bounded index from disk, as on startup.
	if err := s.store.prune(*now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.store.audit(Audit{RequestID: uuid.NewString(), At: *now, Outcome: "read"}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.store.entries("audit")
	if err != nil || len(entries) > 10000 {
		t.Fatalf("audit overflow: %d %v", len(entries), err)
	}
	if s.store.dropped < 5 {
		t.Fatalf("missing retention count: %d", s.store.dropped)
	}
}

func TestCaptureAuditHasSafeSnapshotMetadata(t *testing.T) {
	s, _, _, _, _ := fixture(t)
	g := grant(t, s)
	record, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]json.RawMessage
	if err := s.store.read("audit", record.RequestID, &row); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(record.Snapshot)
	for key, want := range map[string]string{"windowId": `"window-1"`, "bytes": fmt.Sprint(len(encoded)), "partial": "true", "redactionSchemaVersion": "1"} {
		if string(row[key]) != want {
			t.Errorf("audit %s=%s, want %s", key, row[key], want)
		}
	}
	for _, forbidden := range []string{"snapshot", "token", "path", "message", "content"} {
		if _, ok := row[forbidden]; ok {
			t.Errorf("sensitive field %s entered audit", forbidden)
		}
	}
}
