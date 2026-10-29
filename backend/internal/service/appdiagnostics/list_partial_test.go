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

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestListsReportPartialForEachCallNotLifetimeErrorCount(t *testing.T) {
	s, _, _, _, _ := fixture(t)
	g := grant(t, s)
	record, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	if err != nil {
		t.Fatal(err)
	}
	restore := failDocumentRead(s.store, record.ID, syscall.EIO)
	for _, partial := range []bool{true, false} {
		if !partial {
			restore()
		}
		list, err := s.List(context.Background(), g.Token)
		if err != nil {
			t.Fatal(err)
		}
		audit, err := s.Audits(context.Background(), g.Token)
		if err != nil {
			t.Fatal(err)
		}
		for _, result := range []struct {
			value   any
			partial bool
		}{{list, partial}, {audit, false}} {
			value := result.value
			raw, _ := json.Marshal(value)
			var response struct {
				Partial *bool  `json:"partial"`
				Count   uint64 `json:"readErrorCount"`
			}
			if err := json.Unmarshal(raw, &response); err != nil || response.Partial == nil || *response.Partial != result.partial || response.Count == 0 {
				t.Fatalf("partial must describe this call while count stays cumulative: %s %v", raw, err)
			}
		}
	}
}

func TestRevokeDistinguishesPermanentCleanupFailure(t *testing.T) {
	s, _, _, _, dir := fixture(t)
	g := grant(t, s)
	if _, err := s.Capture(context.Background(), g.Token, CaptureRequest{}); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "diagnostics", "snapshots")
	if err := os.Rename(name, name+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = s.Revoke(context.Background(), g.Token)
	rows, err := documents[Audit](s.store, "audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.value.GrantID == g.ID && row.value.Outcome == "revoked" {
			if row.value.ReasonCode != "APP_DIAGNOSTICS_CLEANUP_FAILED" {
				t.Fatalf("permanent cleanup failure presented as retryable: %s", row.value.ReasonCode)
			}
			return
		}
	}
	t.Fatal("revocation audit missing")
}

func TestRendererReloadSuspendsCaptureWithoutRevokingConsent(t *testing.T) {
	s, _, rt, _, _ := fixture(t)
	g := grant(t, s)
	rt.state.Ready = false // The authenticated channel/epoch survives a renderer reload.
	rt.state.Windows = nil
	_, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	var typed *Error
	if !errors.As(err, &typed) || typed.Status != 503 {
		t.Fatalf("reload did not suspend capture: %v", err)
	}
	rt.state.Ready = true
	rt.state.Windows = []Window{{ID: "window-1", Generation: 2}}
	if _, err := s.Status(context.Background(), g.Token); err != nil {
		t.Fatalf("reload revoked unexpired consent: %v", err)
	}
}

func TestRevokeResultShowsCleanupState(t *testing.T) {
	for _, state := range []string{"complete", "pending", "failed"} {
		t.Run(state, func(t *testing.T) {
			s, _, _, _, dir := fixture(t)
			g := grant(t, s)
			record, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "pending":
				failDocumentRead(s.store, record.ID, syscall.EIO)
			case "failed":
				name := filepath.Join(dir, "diagnostics", "snapshots")
				if err := os.Rename(name, name+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.RevokeID(context.Background(), g.Token, g.ID)
			if err != nil || !result.Revoked || result.Cleanup != state {
				t.Fatalf("cleanup status: %+v %v", result, err)
			}
		})
	}
}
func TestCollectorSecretsCannotReachStorageOrAudit(t *testing.T) {
	s, _, rt, _, dir := fixture(t)
	g := grant(t, s)
	raw, err := os.ReadFile("../../../../test/fixtures/app-diagnostics-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot domain.AppDiagnosticsSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Collector.Errors.Records = []domain.AppDiagnosticsErrorSummary{{At: s.now(), Source: "console_error", Name: "Error", Message: "canary-secret /private/path", Count: 1}}
	rt.capture = func(context.Context, CaptureRequest) (Snapshot, error) { return snapshot, nil }
	_, err = s.Capture(context.Background(), g.Token, CaptureRequest{})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "APP_DIAGNOSTICS_INVALID_PAYLOAD" {
		t.Fatalf("secret accepted: %v", err)
	}
	if strings.Contains(err.Error(), "canary-secret") {
		t.Fatal("secret in error")
	}
	for _, kind := range []string{"audit", "snapshots", "grants"} {
		entries, err := os.ReadDir(filepath.Join(dir, "diagnostics", kind))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(dir, "diagnostics", kind, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "canary-secret") {
				t.Fatalf("secret persisted in %s", kind)
			}
		}
	}
}

func TestCaptureRejectsCombinedCollectorOverOneMiB(t *testing.T) {
	s, _, rt, _, _ := fixture(t)
	g := grant(t, s)
	raw, err := os.ReadFile("../../../../test/fixtures/app-diagnostics-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot domain.AppDiagnosticsSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	query := snapshot.Collector.Cache.Queries[0]
	row := query.Workspaces[0]
	row.Sessions = make([]domain.AppDiagnosticsSession, 200)
	for i := range row.Sessions {
		row.Sessions[i] = domain.AppDiagnosticsSession{ID: strings.Repeat("x", 128), Kind: "worker", Status: "working", Activity: "active", Column: "building"}
	}
	query.Workspaces = make([]domain.AppDiagnosticsWorkspace, 100)
	for i := range query.Workspaces {
		query.Workspaces[i] = row
	}
	snapshot.Collector.Cache.Queries = []domain.AppDiagnosticsQuery{query}
	rt.capture = func(context.Context, CaptureRequest) (Snapshot, error) { return snapshot, nil }
	_, err = s.Capture(context.Background(), g.Token, CaptureRequest{})
	var typed *Error
	if !errors.As(err, &typed) || typed.Status != 413 {
		t.Fatalf("oversize must be 413: %v", err)
	}
}

func TestReadErrorCountSurvivesRestartWithoutMarkingHealthyCallsPartial(t *testing.T) {
	s, rows, rt, now, dir := fixture(t)
	g := grant(t, s)
	record, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	if err != nil {
		t.Fatal(err)
	}
	restore := failDocumentRead(s.store, record.ID, syscall.EIO)
	before, err := s.List(context.Background(), g.Token)
	if err != nil || !before.Partial || before.ReadErrorCount == 0 {
		t.Fatalf("missing read failure: %+v %v", before, err)
	}
	restore()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(dir, rows, rt, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	next := grant(t, restarted)
	list, err := restarted.List(context.Background(), next.Token)
	if err != nil || list.Partial || list.ReadErrorCount < before.ReadErrorCount {
		t.Fatalf("restart lost count or retained stale partial: %+v %v", list, err)
	}
	audit, err := restarted.Audits(context.Background(), next.Token)
	if err != nil || audit.Partial || audit.ReadErrorCount < before.ReadErrorCount {
		t.Fatalf("audit restart count: %+v %v", audit, err)
	}
}

func TestListPartialDoesNotIncludeAuditReadFailures(t *testing.T) {
	s, _, _, _, _ := fixture(t)
	g := grant(t, s)
	record, err := s.Capture(context.Background(), g.Token, CaptureRequest{})
	if err != nil {
		t.Fatal(err)
	}
	restore := failDocumentRead(s.store, record.RequestID, syscall.EIO)
	defer restore()
	list, err := s.List(context.Background(), g.Token)
	if err != nil || list.Partial || len(list.Records) != 1 || list.ReadErrorCount == 0 {
		t.Fatalf("audit failure changed snapshot completeness: %+v %v", list, err)
	}
	audit, err := s.Audits(context.Background(), g.Token)
	if err != nil || !audit.Partial || audit.ReadErrorCount == 0 {
		t.Fatalf("audit failure not reported: %+v %v", audit, err)
	}
}
