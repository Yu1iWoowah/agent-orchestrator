package hcg

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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
func TestReportAdmissionRejectsPolicyChangeBeforeSourcesReturn(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("HCG_ENGINE_TESTS") != "1" {
		t.Skip("requires explicitly enabled isolated Node fixture engine")
	}
	node := os.Getenv("HCG_TEST_NODE")
	if node == "" {
		t.Fatal("fixed Node required")
	}
	t.Setenv("PATH", filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, aba := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "off_on_ABA"}[aba], func(t *testing.T) {
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
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			go func() { v, err := svc.Report(ctx, id); done <- result{v, err} }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("source query did not pause")
			}
			projects := projectsvc.NewWithDeps(projectsvc.Deps{Store: st, ReportingPolicyChanged: svc.ProjectPolicyChanged})
			values := []bool{false}
			if aba {
				values = append(values, true)
			}
			for _, enabled := range values {
				if _, err := projects.SetConfig(t.Context(), "p", projectsvc.SetConfigInput{Config: domain.ProjectConfig{HCGReporting: domain.HCGReportingConfig{Enabled: &enabled}}}); err != nil {
					t.Fatal(err)
				}
			}
			close(release)
			started := false
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			deadline := time.After(7 * time.Second)
			for {
				select {
				case <-ticker.C:
					paths, _ := filepath.Glob(filepath.Join(root, "hcg", "reporting", "*", "r-*", "home", "started"))
					started = started || len(paths) > 0
				case got := <-done:
					if started {
						t.Error("stale request started the engine after completed policy change")
					}
					if got.err != nil || got.snapshot.Status != "unavailable" || got.snapshot.Reason != "policy_changed" {
						t.Errorf("stale admission result: %+v %v", got.snapshot, got.err)
					}
					return
				case <-deadline:
					t.Fatal("report did not finish")
				}
			}
		})
	}
}
