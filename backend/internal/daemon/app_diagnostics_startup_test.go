package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	appruntime "github.com/aoagents/agent-orchestrator/backend/internal/appdiagnostics"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	appdiag "github.com/aoagents/agent-orchestrator/backend/internal/service/appdiagnostics"
)

type diagnosticStartupSessions struct{}

func (diagnosticStartupSessions) Get(_ context.Context, id domain.SessionID) (domain.Session, error) {
	return domain.Session{SessionRecord: domain.SessionRecord{ID: id, ProjectID: "fixture-project", Kind: domain.KindOrchestrator, Metadata: domain.SessionMetadata{ControllerGeneration: "fixture-launch"}}}, nil
}

type diagnosticStartupRuntime struct{ now *time.Time }

func (diagnosticStartupRuntime) State() appdiag.RuntimeState {
	return appdiag.RuntimeState{Ready: true, InstanceID: "fixture-app", Epoch: "fixture-epoch", Windows: []appdiag.Window{{ID: "fixture-window", Generation: 1}}}
}
func (r diagnosticStartupRuntime) Capture(_ context.Context, in appdiag.CaptureRequest) (appdiag.Snapshot, error) {
	return appdiag.Snapshot{SchemaVersion: 1, InstanceID: "fixture-app", WindowID: in.WindowID, Generation: 1, CapturedAt: *r.now, Completeness: "partial"}, nil
}

func TestAppDiagnosticsStartupCleansBeforeEnablingAPI(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "without-token"
		if enabled {
			name = "with-token"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now().Add(-8 * 24 * time.Hour)
			// Generate old files through the real service, including Windows owner/ACL
			// handling. No process, network, external account, or manual permission bypass.
			seed, err := appdiag.New(dir, diagnosticStartupSessions{}, diagnosticStartupRuntime{&now}, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer seed.Close()
			id, err := seed.GrantRuntime(context.Background(), "fixture-orchestrator", "fixture-epoch")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "diagnostics", "grants", id+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var credential appdiag.Credential
			if err := json.Unmarshal(raw, &credential); err != nil {
				t.Fatal(err)
			}
			if _, err := seed.Capture(context.Background(), credential.Token, appdiag.CaptureRequest{}); err != nil {
				t.Fatal(err)
			}
			// A recent denied attempt adds an audit without refreshing the old grant.
			now = time.Now()
			if _, err := seed.Capture(context.Background(), "ungranted-fixture", appdiag.CaptureRequest{}); err == nil {
				t.Fatal("fixture unexpectedly authorized")
			}
			entries, err := os.ReadDir(filepath.Join(dir, "diagnostics", "audit"))
			if err != nil {
				t.Fatal(err)
			}
			var expired, recent []string
			for _, entry := range entries {
				raw, err := os.ReadFile(filepath.Join(dir, "diagnostics", "audit", entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				var row appdiag.Audit
				if err := json.Unmarshal(raw, &row); err != nil {
					t.Fatal(err)
				}
				if row.Outcome == "denied" {
					recent = append(recent, entry.Name())
				} else {
					expired = append(expired, entry.Name())
				}
			}
			if len(expired) == 0 || len(recent) != 1 {
				t.Fatal("incomplete retention fixture")
			}
			// Run uses this same initializer unconditionally, before binding listeners.
			service, api, err := newAppDiagnosticsServices(dir, nil, appruntime.New("fixture-runtime-token"), enabled)
			if err != nil {
				t.Fatal(err)
			}
			if service != nil {
				defer service.Close()
			}
			for _, folder := range []string{"grants", "snapshots"} {
				entries, err := os.ReadDir(filepath.Join(dir, "diagnostics", folder))
				if err != nil || len(entries) != 0 {
					t.Fatalf("startup retained %s: %v %v", folder, entries, err)
				}
			}
			if service == nil {
				t.Fatal("storage cleanup disabled without token")
			}
			if (api != nil) != enabled {
				t.Fatal("API availability does not follow trusted channel")
			}
			for _, file := range expired {
				if _, err := os.Stat(filepath.Join(dir, "diagnostics", "audit", file)); !os.IsNotExist(err) {
					t.Fatalf("expired audit retained: %v", err)
				}
			}
			for _, file := range recent {
				if _, err := os.Stat(filepath.Join(dir, "diagnostics", "audit", file)); err != nil {
					t.Fatalf("recent audit lost: %v", err)
				}
			}
		})
	}
}
