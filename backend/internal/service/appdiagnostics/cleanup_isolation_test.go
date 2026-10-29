package appdiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

type cleanupFailureFixture struct {
	s            *Service
	owner, other Credential
	kept, bad    Record
	restore      func()
}

// Retain the auditor's exact-file fault and healthy-data controls. The second
// grant expires later so it remains valid when the first crosses its deadline.
func captureWithBlockedCleanup(t *testing.T, cause string) cleanupFailureFixture {
	t.Helper()
	s, _, _, now, dir := fixture(t)
	owner := grant(t, s)
	*now = now.Add(time.Minute)
	other := grant(t, s)
	kept, err := s.Capture(context.Background(), other.Token, CaptureRequest{})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(3 * time.Second)
	if cause == "expired" {
		*now = owner.ExpiresAt.Add(-time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	write, remove := s.store.writeContent, s.store.removeFile
	var bad Record
	s.store.writeContent = func(f *os.File, data []byte) (int, error) {
		n, err := write(f, data)
		if filepath.Base(filepath.Dir(f.Name())) == "snapshots" && bad.ID == "" {
			if err := json.Unmarshal(data, &bad); err != nil {
				t.Fatal(err)
			}
		}
		var audit Audit
		if json.Unmarshal(data, &audit) == nil && audit.Outcome == "completed" {
			if cause == "expired" {
				*now = owner.ExpiresAt.Add(time.Millisecond)
			} else {
				cancel()
			}
		}
		return n, err
	}
	s.store.removeFile = func(name string) error {
		if bad.ID != "" && name == filepath.Join("snapshots", bad.ID+".json") {
			return fmt.Errorf("CANARY_UNLINK_SECRET: %w", syscall.EACCES)
		}
		return remove(name)
	}
	restore := func() { s.store.writeContent = write; s.store.removeFile = remove }
	t.Cleanup(restore)
	value, err := s.Capture(ctx, owner.Token, CaptureRequest{})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "APP_DIAGNOSTICS_CLEANUP_FAILED" || !reflect.ValueOf(value).IsZero() {
		t.Fatalf("cleanup failure not surfaced safely: %v", err)
	}
	if strings.Contains(err.Error(), "CANARY") || bad.ID == "" {
		t.Fatal("fault was not safely exercised")
	}
	if _, err := os.Stat(filepath.Join(dir, "diagnostics", "snapshots", bad.ID+".json")); err != nil {
		t.Fatalf("unlink fault did not retain file: %v", err)
	}
	return cleanupFailureFixture{s, owner, other, kept, bad, restore}
}

func TestOtherGrantReadsSurviveRejectedSnapshotCleanupFailure(t *testing.T) {
	for _, method := range []string{"list", "audit", "runtime-audit"} {
		t.Run(method, func(t *testing.T) {
			f := captureWithBlockedCleanup(t, "cancelled")
			if _, err := f.s.Status(context.Background(), f.other.Token); err != nil {
				t.Fatalf("other grant invalid before probe: %v", err)
			}
			kept, err := f.s.Read(context.Background(), f.other.Token, f.kept.ID)
			if err != nil || kept.ID != f.kept.ID {
				t.Fatalf("other data unhealthy before probe: %v", err)
			}
			if err := f.s.Sweep(context.Background()); err == nil || strings.Contains(err.Error(), "CANARY") {
				t.Fatalf("background cleanup failure not safely observable: %v", err)
			}
			call := func() (any, error) {
				switch method {
				case "list":
					return f.s.List(context.Background(), f.other.Token)
				case "audit":
					return f.s.Audits(context.Background(), f.other.Token)
				default:
					return f.s.AuditRuntime(context.Background(), "epoch-1", f.other.ID)
				}
			}
			blockedValue, blockedErr := call()
			f.restore()
			if err := f.s.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			recovered, recoveredErr := call()
			if recoveredErr != nil || reflect.ValueOf(recovered).IsZero() {
				t.Fatalf("positive control did not recover: %v", recoveredErr)
			}
			if blockedErr != nil {
				t.Fatalf("other grant unavailable due to unrelated cleanup: %v", blockedErr)
			}
			switch value := blockedValue.(type) {
			case SnapshotList:
				if len(value.Records) != 1 || value.Records[0].ID != f.kept.ID || value.Partial {
					t.Fatalf("healthy grant's snapshot list changed: %+v", value)
				}
			case AuditLog:
				if len(value.Records) == 0 || value.Partial {
					t.Fatal("healthy audit lost or marked partial")
				}
				completed := false
				for _, row := range value.Records {
					if row.GrantID != f.other.ID {
						t.Fatal("audit crossed grant boundary")
					}
					completed = completed || row.Outcome == "completed"
				}
				if !completed {
					t.Fatal("healthy capture missing from audit")
				}
			case json.RawMessage:
				var result struct {
					Records []struct{ Outcome string } `json:"records"`
					Partial bool                       `json:"partial"`
				}
				if err := json.Unmarshal(value, &result); err != nil || result.Partial || len(result.Records) == 0 {
					t.Fatalf("healthy Settings audit unavailable: %v", err)
				}
				completed := false
				for _, row := range result.Records {
					if row.Outcome == "failed" {
						t.Fatal("unrelated cleanup failure entered Settings audit")
					}
					completed = completed || row.Outcome == "completed"
				}
				if !completed {
					t.Fatal("healthy capture missing from Settings audit")
				}
			}
		})
	}
}

func TestCleanupFailureKeepsDeliveryRejectionReason(t *testing.T) {
	for _, cause := range []string{"cancelled", "expired"} {
		t.Run(cause, func(t *testing.T) {
			f := captureWithBlockedCleanup(t, cause)
			code := "APP_SNAPSHOT_TIMEOUT"
			if cause == "expired" {
				code = "APP_DIAGNOSTICS_GRANT_EXPIRED"
			}
			var audit Audit
			if err := f.s.store.read("audit", f.bad.RequestID, &audit); err != nil || audit.Outcome != "failed" || audit.ReasonCode != code {
				t.Fatalf("cleanup replaced rejection reason: outcome=%s reason=%s err=%v", audit.Outcome, audit.ReasonCode, err)
			}
		})
	}
}
