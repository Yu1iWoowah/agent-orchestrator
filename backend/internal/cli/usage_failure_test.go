package cli

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	hcgsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/hcg"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

func TestUsageReportingSameCallerFailures(t *testing.T) {
	if os.Getenv("HCG_ENGINE_TESTS") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires explicitly enabled isolated Node fixture engine")
	}
	node := os.Getenv("HCG_TEST_NODE")
	if node == "" {
		t.Fatal("fixed Node required")
	}
	t.Setenv("PATH", filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"))
	const event = `{"source":"claude","sessionId":"native-test","timestamp":"2026-11-09T00:00:00Z","inputTokens":100,"cacheReadTokens":30,"cacheWriteTokens":40,"outputTokens":20,"reasoningTokens":0,"totalTokens":190,"costMode":"estimated","repoRoot":"SYNTHETIC_PRIVATE"}`
	for _, tc := range []struct{ name, code, reason string }{
		{"nonzero", `process.exit(7)`, "engine_execution_failed"},
		{"schema", `console.log('{}')`, "event_schema_or_identity_mismatch"},
		{"timeout", `setInterval(()=>{},1000)`, "deadline_exceeded"},
		{"inflight_host_stop", `require('node:fs').writeFileSync('/home/report/started','1');setInterval(()=>{},1000)`, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			if err := os.MkdirAll(cfg.dataDir, 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "native.jsonl")
			if err := os.WriteFile(source, []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			on := true
			count := int64(170)
			store := &reportingFixtureStore{session: domain.SessionRecord{ID: "s", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{ProviderConversationID: "native-test", AgentSettingsLaunchID: "launch-1", ControllerGeneration: "launch-1"}}, project: domain.ProjectRecord{ID: "p", Config: domain.ProjectConfig{HCGReporting: domain.HCGReportingConfig{Enabled: &on}}}, path: source, ledger: []domain.UsageLedgerEvent{{ID: 1, BindingID: 1, UsageSourceID: 1, ModelUsageEvent: domain.ModelUsageEvent{SourceEventKey: "canonical-1", Tokens: domain.UsageTokenMetrics{InputTokens: &count}}}}}
			unchanged := func() []byte {
				b, err := json.Marshal([]any{store.session, store.project.Config, store.ledger})
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			before := unchanged()
			svc := hcgsvc.New(t.Context(), cfg.dataDir, store)
			t.Cleanup(svc.Close)
			router := httpd.NewRouterWithControl(config.Config{DataDir: cfg.dataDir}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{HCGReporting: svc}, httpd.ControlDeps{})
			srv := newFixtureServer(t, router)
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			integration := func(args ...string) string {
				t.Helper()
				out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, append([]string{"usage", "integration"}, args...)...)
				if err != nil {
					t.Fatalf("integration %v: %v %s", args, err, out)
				}
				return out
			}
			pkg := filepath.Join(t.TempDir(), "node_modules", "llm-usage-metrics")
			if err := os.MkdirAll(filepath.Join(pkg, "dist"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"llm-usage-metrics","version":"0.14.3"}`), 0600); err != nil {
				t.Fatal(err)
			}
			executable := filepath.Join(pkg, "dist", "bin.js")
			code := `if(process.argv.includes('--version')){console.log('0.14.3')}else{console.log(` + "`" + event + "`" + `);` + tc.code + `}`
			if err := os.WriteFile(executable, []byte(code), 0600); err != nil {
				t.Fatal(err)
			}
			integration("register", "--engine", reporting.EngineName, "--executable", executable, "--version", reporting.EngineVersion)
			integration("clear", "--session", "s")
			readPolicy := func() reporting.View {
				t.Helper()
				raw := integration("get", "--session", "s")
				var v reporting.View
				if err := json.Unmarshal([]byte(raw), &v); err != nil {
					t.Fatal(err)
				}
				return v
			}
			policy := readPolicy()
			type result struct {
				out string
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "usage", "report", "--session", "s", "--engine", reporting.EngineName, "--json")
				done <- result{out, err}
			}()
			if tc.name == "inflight_host_stop" {
				deadline := time.Now().Add(5 * time.Second)
				for {
					paths, _ := filepath.Glob(filepath.Join(cfg.dataDir, "hcg", "reporting", "*", "r-*", "home", "started"))
					if len(paths) > 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("fixture engine did not start")
					}
					time.Sleep(time.Millisecond)
				}
				integration("emergency-stop", "--enabled", "true")
			}
			var got result
			select {
			case got = <-done:
			case <-time.After(40 * time.Second):
				t.Fatal("bounded report did not return")
			}
			var snapshot reporting.Snapshot
			if ExitCode(got.err) != 1 || json.Unmarshal([]byte(got.out), &snapshot) != nil || snapshot.Source != "external_analysis" || snapshot.Status != "unavailable" || snapshot.Reason != tc.reason || len(snapshot.Events) != 0 || len(snapshot.Tools) != 0 || strings.Contains(got.out, "SYNTHETIC_PRIVATE") {
				t.Fatalf("unsafe failure result: exit=%d err=%v output=%s", ExitCode(got.err), got.err, got.out)
			}
			if tc.name == "inflight_host_stop" {
				stopped := readPolicy()
				if !stopped.EmergencyStop || !stopped.Requested || stopped.EffectiveEnabled {
					t.Fatalf("stop changed requested policy: %+v", stopped)
				}
				integration("emergency-stop", "--enabled", "false")
			}
			after := readPolicy()
			if !reflect.DeepEqual(policy.Inputs, after.Inputs) || policy.SessionOverride != after.SessionOverride || policy.Requested != after.Requested || policy.EffectiveEnabled != after.EffectiveEnabled || string(before) != string(unchanged()) {
				t.Fatalf("report failure changed native session/ledger/policy: before=%+v after=%+v", policy, after)
			}
		})
	}
}
