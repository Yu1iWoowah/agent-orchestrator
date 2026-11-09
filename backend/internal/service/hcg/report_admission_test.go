package hcg

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	engine "github.com/aoagents/agent-orchestrator/backend/internal/devimport"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	devimportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/devimport"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

type pausedReportSources struct {
	Store
	session          domain.SessionRecord
	source           string
	entered, release chan struct{}
}

func (s pausedReportSources) ListUsageBindingsForSession(context.Context, domain.SessionID) ([]domain.UsageBindingRecord, error) {
	return []domain.UsageBindingRecord{{ID: 1, SessionID: s.session.ID, Harness: s.session.Harness, NativeRootID: s.session.Metadata.ProviderConversationID}}, nil
}
func (s pausedReportSources) ListUsageSourcesForBinding(ctx context.Context, _ int64) ([]domain.UsageSourceRecord, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []domain.UsageSourceRecord{{Kind: domain.UsageSourceCodexRollout, NativeSessionID: s.session.Metadata.ProviderConversationID, ArtifactPath: s.source}}, nil
}

// The source snapshot is synthetic; only the real report caller and sandboxed
// fixture engine run. Import uses the same service/adapter as daemon wiring.
type reportImportSource struct {
	engine.Store
	row domain.ProjectRecord
}

func (s reportImportSource) ListProjects(context.Context) ([]domain.ProjectRecord, error) {
	return []domain.ProjectRecord{s.row}, nil
}
func (s reportImportSource) ListWorkspaceRepos(context.Context, string) ([]domain.WorkspaceRepoRecord, error) {
	return nil, nil
}
func (s reportImportSource) Close() error { return nil }

func TestReportAdmissionPolicyWriters(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_ENGINE_TESTS") != "1" {
		t.Skip("requires explicitly enabled isolated Node fixture engine")
	}
	node := os.Getenv("HCG_TEST_NODE")
	if node == "" {
		t.Fatal("fixed Node required")
	}
	t.Setenv("PATH", filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, cause := range []string{"project", "settings", "patch", "role", "import", "host", "session", "session_clear", "engine"} {
		for _, inflight := range []bool{false, true} {
			for _, aba := range []bool{false, true} {
				t.Run(cause+"/"+map[bool]string{false: "pending", true: "inflight"}[inflight]+"/"+map[bool]string{false: "off", true: "off_on_ABA"}[aba], func(t *testing.T) {
					st, svc, root, id := launchFixture(t)
					pkg := filepath.Join(t.TempDir(), "node_modules", "llm-usage-metrics")
					if err := os.MkdirAll(filepath.Join(pkg, "dist"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"llm-usage-metrics","version":"0.14.3"}`), 0600); err != nil {
						t.Fatal(err)
					}
					executable := filepath.Join(pkg, "dist", "bin.js")
					code := `if(process.argv.includes('--version')){console.log('0.14.3')}else{require('node:fs').writeFileSync('/home/report/started','1');setInterval(()=>{},1000)}`
					if err := os.WriteFile(executable, []byte(code), 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := svc.Register(t.Context(), executable, reporting.EngineVersion); err != nil {
						t.Fatal(err)
					}
					rec, _, err := st.GetSession(t.Context(), id)
					if err != nil {
						t.Fatal(err)
					}
					on := true
					if err := svc.core.SetOverride(t.Context(), svc.identity(rec), reporting.Inputs{Project: &on}, nil); err != nil {
						t.Fatal(err)
					}
					source := filepath.Join(t.TempDir(), "native.jsonl")
					if err := os.WriteFile(source, []byte("{}\n"), 0600); err != nil {
						t.Fatal(err)
					}
					entered, release := make(chan struct{}), make(chan struct{})
					svc.store = pausedReportSources{Store: st, session: rec, source: source, entered: entered, release: release}
					type result struct {
						snapshot reporting.Snapshot
						err      error
					}
					done := make(chan result, 1)
					ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
					defer cancel()
					go func() { v, e := svc.Report(ctx, id); done <- result{v, e} }()
					select {
					case <-entered:
					case <-ctx.Done():
						t.Fatal("source query did not pause")
					}
					marker := func() bool {
						paths, _ := filepath.Glob(filepath.Join(root, "hcg", "reporting", "*", "r-*", "home", "started"))
						return len(paths) > 0
					}
					if inflight {
						close(release)
						waitLaunch(t, marker)
					}
					projects := projectsvc.NewWithDeps(projectsvc.Deps{Store: st, ReportingPolicyChanged: svc.ProjectPolicyChanged})
					sourceDir := t.TempDir()
					values := []bool{false}
					if aba {
						values = append(values, true)
					}
					for step, enabled := range values {
						var err error
						cfg := domain.ProjectConfig{HCGReporting: domain.HCGReportingConfig{Enabled: &enabled}}
						switch cause {
						case "host":
							err = svc.EmergencyStop(t.Context(), !enabled)
						case "session", "session_clear":
							value := &enabled
							if cause == "session_clear" && enabled {
								value = nil
							}
							_, err = svc.SetOverride(t.Context(), id, value)
						case "engine":
							_, err = svc.Register(t.Context(), executable, reporting.EngineVersion)
						case "import":
							row, _, e := st.GetProject(t.Context(), "p")
							if e != nil {
								t.Fatal(e)
							}
							row.Config = cfg
							importer := devimportsvc.New(devimportsvc.Deps{Store: projects.ImportTarget(st), TargetDataDir: root, OpenSource: func(context.Context, string) (devimportsvc.SourceStore, error) {
								return reportImportSource{Store: st, row: row}, nil
							}})
							_, err = importer.RunProjects(t.Context(), devimportsvc.RunInput{SourceDataDir: sourceDir})
						case "settings":
							_, err = projects.UpdateSettings(t.Context(), "p", projectsvc.UpdateSettingsInput{DisplayName: "project", Config: cfg})
						case "patch":
							var expected any
							if step > 0 {
								expected = false
							}
							_, err = projects.PatchSettings(t.Context(), "p", projectsvc.PatchSettingsInput{Config: map[string]any{"worker": map[string]any{"hcgReporting": map[string]any{"enabled": enabled}}}, Expected: map[string]any{"config": map[string]any{"worker": map[string]any{"hcgReporting": map[string]any{"enabled": expected}}}}})
						default:
							if cause == "role" {
								cfg.HCGReporting.Enabled = &on
								cfg.Worker.HCGReporting.Enabled = &enabled
							}
							_, err = projects.SetConfig(t.Context(), "p", projectsvc.SetConfigInput{Config: cfg})
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					if !inflight {
						close(release)
					}
					started := false
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-ticker.C:
							started = started || marker()
						case got := <-done:
							if !inflight && started {
								t.Error("stale request started engine after completed policy change")
							}
							reason := "policy_changed"
							if inflight {
								reason = "cancelled"
							}
							if got.err != nil || got.snapshot.Status != "unavailable" || got.snapshot.Reason != reason || len(got.snapshot.Events) != 0 || len(got.snapshot.Tools) != 0 {
								t.Errorf("stale report: %+v %v", got.snapshot, got.err)
							}
							if ctx.Err() != nil {
								t.Error("report waited for caller deadline instead of invalidation")
							}
							return
						case <-ctx.Done():
							t.Fatal("report did not finish after invalidation")
						}
					}
				})
			}
		}
	}
}
