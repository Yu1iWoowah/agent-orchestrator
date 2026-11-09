package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	hcgsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/hcg"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

type reportingFixtureStore struct {
	session     domain.SessionRecord
	project     domain.ProjectRecord
	path        string
	sourceReads int
	ledger      []domain.UsageLedgerEvent
}

func (s *reportingFixtureStore) ListProjects(context.Context) ([]domain.ProjectRecord, error) {
	return []domain.ProjectRecord{s.project}, nil
}
func (s *reportingFixtureStore) ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error) {
	return []domain.SessionRecord{s.session}, nil
}
func (s *reportingFixtureStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	return s.session, id == s.session.ID, nil
}
func (s *reportingFixtureStore) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	return s.project, true, nil
}
func (s *reportingFixtureStore) SessionAgentSettings(context.Context, domain.SessionID) (domain.SessionAgentSettings, error) {
	return domain.SessionAgentSettings{ProviderConversationID: "native-test"}, nil
}
func (s *reportingFixtureStore) GetUsageEventsThroughID(context.Context, domain.SessionID) (int64, error) {
	return 1, nil
}
func (s *reportingFixtureStore) ListUsageEventsPage(_ context.Context, _ domain.SessionID, after, _, _ int64) ([]domain.UsageLedgerEvent, error) {
	if after != 0 {
		return nil, nil
	}
	return s.ledger, nil
}
func (s *reportingFixtureStore) ListUsageBindingsForSession(context.Context, domain.SessionID) ([]domain.UsageBindingRecord, error) {
	return []domain.UsageBindingRecord{{ID: 1, SessionID: s.session.ID, Harness: s.session.Harness, NativeRootID: "native-test"}}, nil
}
func (s *reportingFixtureStore) ListUsageSourcesForBinding(context.Context, int64) ([]domain.UsageSourceRecord, error) {
	s.sourceReads++
	return []domain.UsageSourceRecord{{ID: 1, BindingID: 1, Kind: domain.UsageSourceClaudeMain, NativeSessionID: "native-test", ArtifactPath: s.path}}, nil
}

func TestUsageCommandValidation(t *testing.T) {
	for _, args := range [][]string{
		{"report"}, {"report", "--session", "s", "--engine", "other"},
		{"integration", "set", "--session", "s"}, {"integration", "set", "--session", "s", "--enabled", "1"},
		{"integration", "clear"}, {"integration", "emergency-stop"},
		{"integration", "register", "--engine", "llm-usage-metrics", "--executable", "/binary", "--version", "latest"},
	} {
		_, _, err := executeCLI(t, Deps{}, append([]string{"usage"}, args...)...)
		if ExitCode(err) != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

// This deliberately uses one real CLI → HTTP → service → fixed engine caller.
// The read-only store supplies a matching synthetic ledger snapshot; no native
// agent or provider is started and no production database is opened.
func TestUsageReportingSameCaller(t *testing.T) {
	if os.Getenv("HCG_ENGINE_TESTS") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires explicitly enabled fixed isolated engine")
	}
	node, engine := os.Getenv("HCG_TEST_NODE"), os.Getenv("HCG_TEST_ENGINE")
	if node == "" || engine == "" {
		t.Fatal("fixed engine paths required")
	}
	t.Setenv("PATH", filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := setConfigEnv(t)
	if err := os.MkdirAll(cfg.dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "native.jsonl")
	native := `{"type":"assistant","sessionId":"native-test","timestamp":"2026-11-09T00:00:00Z","message":{"id":"msg-1","role":"assistant","model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40},"content":[]}}
`
	if err := os.WriteFile(path, []byte(native), 0600); err != nil {
		t.Fatal(err)
	}
	ptr := func(n int64) *int64 { return &n }
	store := &reportingFixtureStore{session: domain.SessionRecord{ID: "s", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{ProviderConversationID: "native-test", AgentSettingsLaunchID: "launch-1", ControllerGeneration: "launch-1"}}, project: domain.ProjectRecord{ID: "p"}, path: path,
		ledger: []domain.UsageLedgerEvent{{ID: 1, BindingID: 1, UsageSourceID: 1, ModelUsageEvent: domain.ModelUsageEvent{SourceEventKey: "canonical-1", CreatedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), Tokens: domain.UsageTokenMetrics{InputTokens: ptr(170), CachedInputTokens: ptr(70), UncachedInputTokens: ptr(100), OutputTokens: ptr(20)}}}}}
	ledgerBefore, _ := json.Marshal(store.ledger)
	svc := hcgsvc.New(t.Context(), cfg.dataDir, store)
	t.Cleanup(svc.Close)
	router := httpd.NewRouterWithControl(config.Config{DataDir: cfg.dataDir}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{HCGReporting: svc, UsageEvents: usagesvc.NewEventsReader(store)}, httpd.ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()
	writeRunFileFor(t, cfg, srv)
	call := func(wantExit int, args ...string) string {
		t.Helper()
		out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, append([]string{"usage"}, args...)...)
		if ExitCode(err) != wantExit {
			t.Fatalf("%v: exit=%d err=%v output=%s", args, ExitCode(err), err, out)
		}
		return out
	}
	integration := func(args ...string) string { return call(0, append([]string{"integration"}, args...)...) }
	report := func(exit int) reporting.Snapshot {
		t.Helper()
		out := call(exit, "report", "--session", "s", "--engine", reporting.EngineName, "--json")
		var v reporting.Snapshot
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	integration("clear", "--session", "s", "--json")
	// A committed main-session success snapshot feeds this same report caller.
	svc.ReportingLaunchReady(ports.HCGReportingLaunch{Origin: ports.HCGReportingMainChat, Owner: domain.SessionConversationOwner("s"), SessionID: "s", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, DataRoot: cfg.dataDir, CommittedOwner: store.session.ControllerOwner(), Fresh: true})
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, err := svc.Get(t.Context(), "s")
		if err != nil {
			t.Fatal(err)
		}
		if v.ManifestRevision > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("successful launch manifest unavailable to report caller")
		}
		time.Sleep(time.Millisecond)
	}
	if v := report(1); v.Status != "disabled" || store.sourceReads != 0 {
		t.Fatalf("off launched/read a source: %+v", v)
	}
	integration("set", "--session", "s", "--enabled", "true")
	if v := report(1); v.Status != "unavailable" || store.sourceReads != 0 {
		t.Fatalf("unregistered engine: %+v", v)
	}
	integration("register", "--engine", reporting.EngineName, "--executable", engine, "--version", reporting.EngineVersion, "--json")
	v := report(0)
	if v.Source != "external_analysis" || len(v.Events) != 1 || v.Events[0].InputIncludingCache != 170 || v.Events[0].CacheWriteTokens != 40 || v.Events[0].OutputTokens != 20 {
		t.Fatalf("external vector: %+v", v)
	}
	for _, parent := range []bool{false, true} {
		store.project.Config.HCGReporting.Enabled = &parent
		integration("set", "--session", "s", "--enabled", "false")
		if v := report(1); v.Status != "disabled" {
			t.Fatalf("explicit off: %+v", v)
		}
		integration("clear", "--session", "s")
		want := 1
		if parent {
			want = 0
		}
		report(want)
		integration("emergency-stop", "--enabled", "true")
		if v := report(1); v.Status != "disabled" {
			t.Fatalf("stop: %+v", v)
		}
		integration("emergency-stop", "--enabled", "false")
		report(want)
	}
	// Role false beats project true, session true beats role false.
	off := false
	store.project.Config.Worker.HCGReporting.Enabled = &off
	integration("clear", "--session", "s")
	report(1)
	integration("set", "--session", "s", "--enabled", "true")
	report(0)
	integration("emergency-stop", "--enabled", "true")
	report(1)
	integration("emergency-stop", "--enabled", "false")
	report(0)
	// The startup path checks the stored fixed registration, without reinstall.
	restarted := hcgsvc.New(t.Context(), cfg.dataDir, store)
	t.Cleanup(restarted.Close)
	if got, err := restarted.Get(t.Context(), "s"); err != nil || !got.Available || !got.Active {
		t.Fatalf("startup probe: %+v %v", got, err)
	}
	resp, err := http.Get(srv.URL + "/api/v1/usage/sessions/s/events")
	if err != nil {
		t.Fatal(err)
	}
	var canonical controllers.UsageEventsResponse
	err = json.NewDecoder(resp.Body).Decode(&canonical)
	resp.Body.Close()
	if err != nil || len(canonical.Events) != 1 || *canonical.Events[0].InputTokens != v.Events[0].InputIncludingCache || *canonical.Events[0].OutputTokens != v.Events[0].OutputTokens {
		t.Fatalf("same synthetic snapshot comparison: %+v %v", canonical, err)
	}
	ledgerAfter, _ := json.Marshal(store.ledger)
	if string(ledgerAfter) != string(ledgerBefore) {
		t.Fatal("report changed canonical ledger")
	}
	_, _, err = executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "usage", "integration", "get", "--session", "unknown")
	if err == nil || !strings.Contains(err.Error(), "SESSION_NOT_FOUND") || !strings.Contains(err.Error(), "request") {
		t.Fatalf("lost error envelope: %v", err)
	}
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":true,"path":"private"}`, `{"enabled":true} {}`} {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/sessions/s/hcg-reporting/override", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "requestId") {
			t.Fatalf("bad body accepted: %s => %d %s", body, resp.StatusCode, raw)
		}
	}
}
