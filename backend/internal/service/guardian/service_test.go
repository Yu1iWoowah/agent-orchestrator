package guardian

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type fixture struct {
	t       *testing.T
	store   *sqlite.Store
	db      *sql.DB
	root    string
	now     time.Time
	base    time.Time
	session domain.SessionRecord
	svc     *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	store := sqlitetest.MustOpenAt(t, dir)
	ctx := context.Background()
	base := time.Now().UTC().Add(time.Minute)
	must(t, store.UpsertProject(ctx, domain.ProjectRecord{ID: "project", Path: dir, RegisteredAt: base}))
	session, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "project", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: base}, Metadata: domain.SessionMetadata{AgentSessionID: "root", AgentSessionIDLaunchID: "launch", RuntimeLaunchID: "launch"}, CreatedAt: base, UpdatedAt: base})
	must(t, err)
	must(t, store.BindSessionAgentAccount(ctx, session.ID, domain.AgentAccountProviderCodex, domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex), base.Add(-time.Minute)))
	_, err = store.UpsertUsageBinding(ctx, domain.UsageBindingRecord{SessionID: session.ID, Harness: domain.HarnessCodex, NativeRootID: "root", State: domain.UsageBindingActive, UpdatedAt: base})
	must(t, err)
	db, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	_, err = db.Exec("PRAGMA foreign_keys=ON")
	must(t, err)
	root := filepath.Join(dir, "sessions")
	must(t, os.Mkdir(root, 0700))
	f := &fixture{t: t, store: store, db: db, root: root, base: base, now: base.Add(10 * time.Minute), session: session}
	f.svc = New(store, Config{Clock: func() time.Time { return f.now }, Roots: func(context.Context, domain.GuardianOwner) ([]string, error) { return []string{root}, nil }})
	f.write("root", f.meta("root", "", false, base.Add(-time.Hour)))
	return f
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) enable(enabled bool) {
	f.t.Helper()
	_, err := f.store.UpdateProjectSettings(context.Background(), "project", "", domain.ProjectConfig{GuardianObservability: domain.GuardianObservabilityConfig{Enabled: enabled}})
	must(f.t, err)
}
func (f *fixture) meta(id, parent string, guardian bool, at time.Time) string {
	p := map[string]any{"id": id, "cli_version": "0.166.0", "source": "cli"}
	if parent != "" {
		p["parent_thread_id"] = parent
	}
	if guardian {
		p["thread_source"] = "guardian_review"
	}
	return f.line("session_meta", p, at)
}
func (f *fixture) line(kind string, p any, at time.Time) string {
	v, err := json.Marshal(map[string]any{"timestamp": at.Format(time.RFC3339Nano), "type": kind, "payload": p})
	must(f.t, err)
	return string(v) + "\n"
}
func (f *fixture) request(id, turn string, input, cached int64) string {
	return f.line("event_msg", map[string]any{"type": "token_usage_record", "thread_id": "guardian", "turn_id": turn, "response_id": id, "root_turn_id": "shared-root-turn", "usage": map[string]any{"input_tokens": input, "cached_input_tokens": cached, "output_tokens": int64(20), "reasoning_output_tokens": int64(10), "total_tokens": input + 20}}, f.base.Add(2*time.Minute))
}
func (f *fixture) model(turn, model string) string {
	return f.line("turn_context", map[string]any{"turn_id": turn, "model": model}, f.base.Add(2*time.Minute))
}
func (f *fixture) write(id, data string) {
	f.t.Helper()
	must(f.t, os.WriteFile(filepath.Join(f.root, id+".jsonl"), []byte(data), 0600))
}
func (f *fixture) append(id, data string) {
	f.t.Helper()
	file, err := os.OpenFile(filepath.Join(f.root, id+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	must(f.t, err)
	_, err = file.WriteString(data)
	must(f.t, err)
	must(f.t, file.Close())
}
func (f *fixture) collect() { f.t.Helper(); must(f.t, f.svc.Collect(context.Background())) }
func (f *fixture) owners() []domain.GuardianOwner {
	f.t.Helper()
	v, err := f.store.GuardianOwners(context.Background(), f.now)
	must(f.t, err)
	return v
}
func (f *fixture) requests() []domain.GuardianRequest {
	f.t.Helper()
	owners := f.owners()
	if len(owners) == 0 {
		return nil
	}
	v, err := f.store.GuardianRequests(context.Background(), owners[0].EpochID)
	must(f.t, err)
	return v
}
func (f *fixture) codes() map[domain.GuardianCode]int64 {
	f.t.Helper()
	v, err := f.store.GuardianAnomalies(context.Background(), f.session.ID)
	must(f.t, err)
	return v
}
func (f *fixture) count(table string) int64 {
	f.t.Helper()
	var n int64
	must(f.t, f.db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
	return n
}

func TestA1DisabledEnableBoundaryAndStop(t *testing.T) {
	f := newFixture(t)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("before", "turn", 100, 10))
	f.collect()
	if f.count("guardian_epochs") != 0 || f.count("guardian_requests") != 0 {
		t.Fatal("disabled collected facts")
	}
	f.enable(true)
	// This source predates enable, even though its records claim future times.
	old := f.meta("guardian", "root", true, f.base.Add(-time.Hour))
	f.write("guardian", old+f.request("old", "turn", 100, 10))
	f.collect()
	if len(f.requests()) != 0 {
		t.Fatal("old file backfilled")
	}
	f.append("guardian", f.request("new", "turn", 100, 10))
	f.collect()
	if len(f.requests()) != 1 {
		t.Fatal("new append missing")
	}
	f.enable(false)
	f.append("guardian", f.request("disabled", "turn", 100, 10))
	f.collect()
	if f.count("guardian_requests") != 1 {
		t.Fatal("collection continued after disable")
	}
}

func TestA3ResponseDedupLateExactModelAndSpawn(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("response-1", "turn-1", 100, 20)+f.request("response-1", "turn-1", 100, 20)+f.request("response-2", "turn-2", 80, 0))
	f.write("ordinary", f.meta("ordinary", "root", false, f.base)+strings.ReplaceAll(f.request("spawn", "turn-1", 50, 0), "guardian", "ordinary"))
	f.collect()
	r := f.requests()
	if len(r) != 2 || r[0].ModelID != "" || r[1].ModelID != "" {
		t.Fatalf("initial requests=%+v", r)
	}
	f.append("guardian", f.line("turn_context", map[string]any{"thread_id": "foreign", "turn_id": "turn-1", "model": "gpt-5.4"}, f.base.Add(2*time.Minute))+f.model("turn-2", "gpt-5.4-mini"))
	f.collect()
	r = f.requests()
	if r[0].ModelID != "" || r[1].ModelID != "gpt-5.4-mini" {
		t.Fatalf("model copied across turn: %+v", r)
	}
	f.svc = New(f.store, f.svc.cfg)
	f.collect()
	if len(f.requests()) != 2 {
		t.Fatal("restart duplicated requests")
	}
}

func TestA4ConflictRotationTruncationHalfLineAndMissing(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	f.write("guardian", head+f.request("r", "t", 100, 20))
	f.collect()
	half := f.request("half", "t", 70, 10)
	f.append("guardian", half[:len(half)/2])
	f.collect()
	if len(f.requests()) != 1 || f.codes()[domain.GuardianPartialLine] == 0 {
		t.Fatal("partial line advanced")
	}
	f.append("guardian", half[len(half)/2:])
	f.collect()
	if len(f.requests()) != 2 {
		t.Fatal("completed line lost")
	}
	f.append("guardian", f.request("r", "t", 999, 20))
	f.collect()
	r := f.requests()
	if !r[1].Conflict || *r[1].Tokens.Input != 100 || f.codes()[domain.GuardianSourceConflict] == 0 {
		t.Fatalf("conflict overwritten: %+v", r)
	}
	path := filepath.Join(f.root, "guardian.jsonl")
	must(t, os.Rename(path, path+".old"))
	f.write("guardian", head+f.request("r", "t", 100, 20))
	f.collect()
	if len(f.requests()) != 2 || f.codes()[domain.GuardianSourceRotated] == 0 {
		t.Fatal("rotation duplicated or missed")
	}
	f.write("guardian", head)
	f.collect()
	if f.codes()[domain.GuardianSourceTruncated] == 0 {
		t.Fatal("truncation missing")
	}
	must(t, os.Remove(path))
	f.collect()
	if f.codes()[domain.GuardianSourceMissing] == 0 {
		t.Fatal("missing file unreported")
	}
}

func TestA4BudgetVersionsAndInvalidFields(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.cfg.RecordBudget = 1
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r1", "t", 100, 10)+f.request("r2", "t", 100, 10))
	f.collect()
	if len(f.requests()) != 1 || f.codes()[domain.GuardianCoverageGap] == 0 {
		t.Fatal("record budget silent")
	}
	f.collect()
	if len(f.requests()) != 2 {
		t.Fatal("budget lost tail")
	}
	f.svc.cfg.RecordBudget = 512
	f.append("guardian", strings.ReplaceAll(f.request("bad", "t", 100, 10), `"input_tokens":100`, `"input_tokens":9223372036854775808`)+f.line("event_msg", map[string]any{"type": "token_usage_record", "thread_id": "guardian", "turn_id": "t", "response_id": "missing-time"}, time.Time{}))
	f.collect()
	if f.codes()[domain.GuardianInvalidMetadata] == 0 || f.codes()[domain.GuardianUnknownTime] == 0 {
		t.Fatal("invalid counter or missing time silent")
	}
	f.write("unknown", strings.ReplaceAll(f.meta("unknown", "root", true, f.base), "0.166.0", "9.999.0"))
	f.collect()
	if f.codes()[domain.GuardianUnsupportedVersion] == 0 {
		t.Fatal("unknown version silent")
	}
	f.write("missing-version", strings.ReplaceAll(f.meta("missing-version", "root", true, f.base), `"cli_version":"0.166.0",`, ""))
	f.collect()
	if f.codes()[domain.GuardianUnsupportedVersion] < 2 {
		t.Fatal("missing version silent")
	}

	f.svc.cfg.DiscoveryBudget = 1
	f.write("new-a", f.meta("new-a", "root", true, f.base))
	f.write("new-b", f.meta("new-b", "root", true, f.base))
	f.collect()
	if f.codes()[domain.GuardianCoverageGap] < 2 {
		t.Fatal("discovery budget silent")
	}
}

func TestA7A8LedgerIsolationAndSensitiveSentinels(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	o := f.owners()[0]
	ctx := context.Background()
	source, err := f.store.InsertUsageSource(ctx, domain.UsageSourceRecord{BindingID: o.BindingID, Kind: domain.UsageSourceCodexRollout, NativeSessionID: "root", ArtifactPath: filepath.Join(f.root, "root.jsonl"), FileIdentity: "synthetic-ledger", State: domain.UsageSourceActive, UpdatedAt: f.base})
	must(t, err)
	value := func(n int64) *int64 { return &n }
	must(t, f.store.ApplyUsageChunk(ctx, source.ID, 0, source.UpdatedAt, domain.SourceCursorState{ByteOffset: 42, State: domain.UsageSourceActive, UpdatedAt: f.base}, []domain.ModelUsageEvent{{ProviderID: domain.UsageProviderOpenAI, ModelID: "gpt-5.4-mini", MeasurementKind: domain.UsageMeasurementNativeReported, Tokens: domain.UsageTokenMetrics{InputTokens: value(500), CachedInputTokens: value(200), UncachedInputTokens: value(300), OutputTokens: value(50)}, Costs: domain.UsageEventCosts{InputCostNanos: value(10), CachedInputCostNanos: value(2), OutputCostNanos: value(5), EstimatedCostNanos: value(17), PricingVersion: "synthetic"}, SourceEventKey: "ledger-event", CreatedAt: f.base}}))
	beforeAggregate, err := f.store.ListUsageModelAggregates(ctx, f.session.ID)
	must(t, err)
	before := f.count("model_usage_events")
	sentinel := "PRIVATE-U16-DO-NOT-STORE"
	raw := f.request("response", "turn", 100, 30)
	var e map[string]any
	must(t, json.Unmarshal([]byte(raw), &e))
	p := e["payload"].(map[string]any)
	for _, key := range []string{"command", "rawCommand", "argv", "stdin", "cwd", "files", "path", "host", "url", "mcp", "messages", "rationale", "unknown"} {
		p[key] = map[string]any{"nested": sentinel}
	}
	encoded, err := json.Marshal(e)
	must(t, err)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+string(encoded)+"\n"+f.model("turn", "gpt-5.4-mini"))
	f.collect()
	if len(f.requests()) != 1 || f.count("model_usage_events") != before {
		t.Fatal("new diagnostics changed ledger")
	}
	afterAggregate, err := f.store.ListUsageModelAggregates(ctx, f.session.ID)
	must(t, err)
	if !reflect.DeepEqual(beforeAggregate, afterAggregate) {
		t.Fatal("diagnostics repriced or reaggregated ledger")
	}
	var key string
	var cost, offset int64
	must(t, f.db.QueryRow("SELECT source_event_key,estimated_cost_nanos FROM model_usage_events").Scan(&key, &cost))
	must(t, f.db.QueryRow("SELECT byte_offset FROM usage_sources WHERE id=?", source.ID).Scan(&offset))
	if key != "ledger-event" || cost != 17 || offset != 42 {
		t.Fatal("ledger identity/cost/cursor changed")
	}

	for _, table := range []string{"guardian_epochs", "guardian_threads", "guardian_turn_models", "guardian_requests", "guardian_cursors", "guardian_positions", "guardian_anomalies", "guardian_watermarks"} {
		func() {
			rows, err := f.db.Query("SELECT * FROM " + table)
			must(t, err)
			defer rows.Close()
			cols, err := rows.Columns()
			must(t, err)
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				must(t, rows.Scan(ptrs...))
				if strings.Contains(fmt.Sprint(vals), sentinel) {
					t.Fatalf("sentinel stored in %s", table)
				}
			}
			must(t, rows.Err())
		}()
	}
	b, err := json.Marshal(f.requests())
	must(t, err)
	if strings.Contains(string(b), sentinel) {
		t.Fatal("sentinel leaked from DTO")
	}
	must(t, f.store.PurgeGuardian(ctx, f.session.ID, f.now))
	if f.count("model_usage_events") != before {
		t.Fatal("purge deleted ledger")
	}
}

func TestA9RetentionPurgeReplayAndCascade(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	f.write("guardian", head+f.request("r", "t", 100, 20))
	f.collect()
	if len(f.requests()) != 1 {
		t.Fatal("missing fixture")
	}
	f.now = f.base.Add(domain.GuardianRetention + 3*time.Minute)
	f.collect()
	if f.count("guardian_requests") != 0 || f.count("guardian_threads") != 0 || f.count("guardian_cursors") != 0 {
		t.Fatal("expired IDs retained")
	}
	f.collect()
	if f.count("guardian_requests") != 0 {
		t.Fatal("old replay resurrected")
	}
	// Purge advances a durable session floor even when all per-request IDs vanish.
	f.now = f.base.Add(20 * time.Minute)
	must(t, f.store.PurgeGuardian(context.Background(), f.session.ID, f.now))
	f.collect()
	if f.count("guardian_requests") != 0 {
		t.Fatal("purged replay resurrected")
	}
	if _, err := os.Stat(filepath.Join(f.root, "guardian.jsonl")); err != nil {
		t.Fatal("purge touched transcript")
	}
	_, err := f.db.Exec("DELETE FROM change_log WHERE session_id=?", f.session.ID)
	must(t, err)
	_, err = f.db.Exec("DELETE FROM sessions WHERE id=?", f.session.ID)
	must(t, err)
	if f.count("guardian_epochs") != 0 || f.count("guardian_watermarks") != 0 {
		t.Fatal("session cascade incomplete")
	}
	_, err = f.db.Exec("DELETE FROM change_log WHERE project_id='project'")
	must(t, err)
	_, err = f.db.Exec("DELETE FROM projects WHERE id='project'")
	must(t, err)
	if f.count("guardian_project_policy") != 0 {
		t.Fatal("project cascade incomplete")
	}
}

func TestA4CrashBeforeCommitRetriesWithoutLosingCursor(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20))
	_, err := f.db.Exec(`CREATE TRIGGER fail_guardian BEFORE INSERT ON guardian_requests BEGIN SELECT RAISE(ABORT,'synthetic crash'); END`)
	must(t, err)
	if err := f.svc.Collect(context.Background()); err == nil {
		t.Fatal("expected failed transaction")
	}
	if f.count("guardian_cursors") != 0 || f.count("guardian_threads") != 0 {
		t.Fatal("partial batch committed")
	}
	_, err = f.db.Exec("DROP TRIGGER fail_guardian")
	must(t, err)
	f.collect()
	if len(f.requests()) != 1 {
		t.Fatal("replay lost request")
	}
}

func TestA1A4NestedLineageAndOwnerConflicts(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("parent", f.meta("parent", "root", false, f.base))
	f.write("guardian", f.meta("guardian", "parent", true, f.base)+f.request("r", "t", 100, 20))
	f.collect()
	if len(f.requests()) != 1 {
		t.Fatal("nested parent unresolved")
	}
	f.write("guardian", f.meta("guardian", "missing", true, f.base))
	f.collect()
	if f.codes()[domain.GuardianOwnerUnresolved] != 0 {
		t.Fatal("unattributed broken chain counted for owner")
	}
	f.write("guardian", f.meta("guardian", "guardian", true, f.base))
	f.collect()
	if f.codes()[domain.GuardianOwnerConflict] == 0 {
		t.Fatal("cycle silent")
	}
}

// An already parsed batch is rejected if the user disables collection while
// I/O is running. The approval path has no dependency on this result.
func TestA1DisableBetweenReadAndCommit(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	o := f.owners()[0]
	f.enable(false)
	err := f.store.ApplyGuardianBatch(context.Background(), domain.GuardianBatch{Owner: o, Thread: domain.GuardianThread{ThreadID: "guardian", ParentID: "root", CLIVersion: "0.166.0"}, Cursor: domain.GuardianCursor{UpdatedAt: f.now}})
	if !errors.Is(err, domain.ErrGuardianDisabled) {
		t.Fatalf("disabled batch=%v", err)
	}
}

func TestA1AccountEpochAndInterfaceSwitch(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("same", "turn", 100, 20))
	f.collect()
	old := f.owners()[0]
	_, err := f.db.Exec("UPDATE sessions SET session_mode='chat' WHERE id=?", f.session.ID)
	must(t, err)
	f.collect()
	if f.owners()[0].EpochID != old.EpochID || len(f.requests()) != 1 {
		t.Fatal("interface switch invented epoch")
	}
	must(t, f.store.CreateAgentAccount(context.Background(), domain.AgentAccount{ID: "other-account", Provider: domain.AgentAccountProviderCodex, Home: f.root, CreatedAt: f.base, ProfileKind: domain.AgentAccountProfileContained}))
	must(t, f.store.BindSessionAgentAccount(context.Background(), f.session.ID, domain.AgentAccountProviderCodex, "other-account", f.base.Add(5*time.Minute)))
	f.collect()
	newOwner := f.owners()[0]
	if newOwner.EpochID == old.EpochID || len(f.requests()) != 0 {
		t.Fatal("account epochs mixed")
	}
	newer := strings.ReplaceAll(f.request("same", "turn", 77, 10), f.base.Add(2*time.Minute).Format(time.RFC3339Nano), f.base.Add(6*time.Minute).Format(time.RFC3339Nano))
	f.append("guardian", newer)
	f.collect()
	if len(f.requests()) != 1 || *f.requests()[0].Tokens.Input != 77 {
		t.Fatal("new account append missing")
	}
	previous, err := f.store.GuardianRequests(context.Background(), old.EpochID)
	must(t, err)
	if len(previous) != 1 || *previous[0].Tokens.Input != 100 {
		t.Fatal("old scope overwritten")
	}
}

func TestA1OtherProjectCannotClaimSameNativeRoot(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	must(t, f.store.UpsertProject(context.Background(), domain.ProjectRecord{ID: "disabled-project", Path: t.TempDir(), RegisteredAt: f.base}))
	_, err := f.store.CreateSession(context.Background(), domain.SessionRecord{ProjectID: "disabled-project", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.base}, Metadata: domain.SessionMetadata{AgentSessionID: "root"}, CreatedAt: f.base, UpdatedAt: f.base})
	must(t, err)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 10))
	f.collect()
	if f.count("guardian_requests") != 0 || f.codes()[domain.GuardianOwnerConflict] == 0 {
		t.Fatal("ambiguous root attributed")
	}
}

func TestA3A4ModelConflictAndQueueBound(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20)+f.model("t", "gpt-5.4-mini"))
	f.collect()
	f.append("guardian", f.model("t", "gpt-5.4"))
	f.collect()
	r := f.requests()
	if len(r) != 1 || !r[0].Conflict || r[0].ModelID != "" {
		t.Fatal("conflicting model selected")
	}
	// Fill the real durable queue to its documented cap, then offer another turn.
	o := f.owners()[0]
	_, err := f.db.Exec(`WITH RECURSIVE n(x) AS(SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<4095)
 INSERT INTO guardian_turn_models(epoch_id,thread_id,turn_id,model_id,provider_at)
 SELECT ?,'guardian','queued-'||x,'gpt-5.4-mini',? FROM n`, o.EpochID, f.base.Add(2*time.Minute))
	must(t, err)
	f.append("guardian", f.model("overflow", "gpt-5.4-mini"))
	f.collect()
	if f.count("guardian_turn_models") != 4096 || f.codes()[domain.GuardianCoverageGap] == 0 {
		t.Fatal("queue cap silent")
	}
}

func TestA4OversizeBoundAndSymlinkConfinement(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+strings.Repeat("x", 5*maxLine)+"\n"+f.request("r", "t", 100, 20))
	f.collect()
	if f.codes()[domain.GuardianInvalidMetadata] == 0 || f.codes()[domain.GuardianCoverageGap] == 0 {
		t.Fatal("large line unbounded or silent")
	}
	f.collect()
	if len(f.requests()) != 1 {
		t.Fatal("large-line recovery lost valid tail")
	}
	outside := filepath.Join(t.TempDir(), "escape.jsonl")
	must(t, os.WriteFile(outside, []byte(f.meta("escape", "root", true, f.base)), 0600))
	if err := os.Symlink(outside, filepath.Join(f.root, "link.jsonl")); err != nil {
		t.Skip("symlinks unavailable")
	}
	result := (&headerDiscovery{}).scan(context.Background(), []string{f.root}, 4096, nil, f.now)
	index := result.index
	if _, ok := index["escape"]; ok {
		t.Fatal("followed escaping symlink")
	}
	if file, err := openFile(f.root, "link.jsonl"); err == nil {
		_ = file.Close()
		t.Fatal("opened escaping symlink")
	}
}

func TestA9DisabledCleanupAndReenableDoNotRefreshRetention(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20))
	f.collect()
	first := f.requests()[0].FirstSeenAt
	f.append("guardian", f.request("r", "t", 100, 20))
	f.now = f.now.Add(time.Hour)
	f.collect()
	if !f.requests()[0].FirstSeenAt.Equal(first) {
		t.Fatal("replay refreshed first seen")
	}
	f.enable(false)
	f.now = f.base.Add(domain.GuardianRetention + 3*time.Minute)
	f.collect()
	if f.count("guardian_requests") != 0 {
		t.Fatal("disabled prevented expiry")
	}
	f.enable(true)
	f.collect()
	if f.count("guardian_requests") != 0 {
		t.Fatal("reenable resurrected old event")
	}
}

func TestA9ExpiredThreadRetainsOnlyOwnerForNewerRequest(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("old", "t", 100, 20))
	f.collect()
	f.now = f.base.Add(29 * 24 * time.Hour)
	fresh := strings.ReplaceAll(f.request("fresh", "fresh-turn", 100, 20), f.base.Add(2*time.Minute).Format(time.RFC3339Nano), f.now.Add(-time.Minute).Format(time.RFC3339Nano))
	f.append("guardian", fresh)
	f.collect()
	f.now = f.base.Add(domain.GuardianRetention + 3*time.Minute)
	f.collect()
	r := f.requests()
	if len(r) != 1 || r[0].ResponseID != "fresh" || r[0].CLIVersion != "0.166.0" {
		t.Fatal("fresh request lost version")
	}
	var version string
	must(t, f.db.QueryRow("SELECT cli_version FROM guardian_threads").Scan(&version))
	if version != "" {
		t.Fatal("expired thread retained version metadata")
	}
}

func TestA4A9OldSourcePositionSurvivesRestartWithoutNativeIDs(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base.Add(-time.Hour))+f.request("old", "t", 100, 20))
	f.collect()
	if f.count("guardian_requests") != 0 || f.count("guardian_threads") != 0 || f.count("guardian_cursors") != 0 || f.count("guardian_positions") != 1 {
		t.Fatal("old source retained native IDs or lost position")
	}
	f.append("guardian", f.request("new", "t", 70, 10))
	f.svc = New(f.store, f.svc.cfg)
	f.collect()
	r := f.requests()
	if len(r) != 1 || r[0].ResponseID != "new" {
		t.Fatal("restart lost new append")
	}
}

func TestA9ProviderOffsetNormalizesToUTCRetention(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	at := f.base.Add(2 * time.Minute)
	raw := strings.ReplaceAll(f.request("r", "t", 100, 20), at.Format(time.RFC3339Nano), at.In(time.FixedZone("UTC+8", 8*60*60)).Format(time.RFC3339Nano))
	f.write("guardian", f.meta("guardian", "root", true, f.base)+raw)
	f.collect()
	r := f.requests()
	if len(r) != 1 || r[0].ProviderAt.Location() != time.UTC {
		t.Fatal("provider timestamp not stored in UTC")
	}
	f.now = f.base.Add(domain.GuardianRetention + 3*time.Minute)
	f.collect()
	if f.count("guardian_requests") != 0 {
		t.Fatal("provider offset extended retention")
	}
}

func TestGuardianCDCPurgeAndAnomaliesContainOnlyAOIdentity(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("response", "turn", 100, 20))
	f.collect()
	seq, err := f.store.LatestSeq(context.Background())
	must(t, err)
	must(t, f.store.PurgeGuardian(context.Background(), f.session.ID, f.now))
	after, err := f.store.LatestSeq(context.Background())
	must(t, err)
	if after <= seq {
		t.Fatal("purge emitted no CDC")
	}
	must(t, f.store.RecordGuardianAnomaly(context.Background(), f.session.ID, domain.GuardianCoverageGap, f.now))
	latest, err := f.store.LatestSeq(context.Background())
	must(t, err)
	if latest <= after {
		t.Fatal("anomaly emitted no CDC")
	}
	rows, err := f.db.Query("SELECT payload FROM change_log WHERE seq>?", seq)
	must(t, err)
	defer rows.Close()
	expected := fmt.Sprintf(`{"id":%q}`, f.session.ID)
	for rows.Next() {
		var payload string
		must(t, rows.Scan(&payload))
		if payload != expected {
			t.Fatalf("CDC outside whitelist: %s", payload)
		}
	}
	must(t, rows.Err())
}

func TestA4ReusedInodeWithNewHeaderDoesNotSkipNewThread(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("old", "t", 100, 20))
	f.collect()
	path := filepath.Join(f.root, "guardian.jsonl")
	before, err := os.Stat(path)
	must(t, err)
	raw := strings.ReplaceAll(f.request("new", "t", 77, 10), `"thread_id":"guardian"`, `"thread_id":"guardian-2"`)
	f.write("guardian", f.meta("guardian-2", "root", true, f.base.Add(time.Second))+raw)
	after, err := os.Stat(path)
	must(t, err)
	if !os.SameFile(before, after) {
		t.Fatal("fixture did not preserve inode")
	}
	f.collect()
	if len(f.requests()) != 2 {
		t.Fatal("old physical position skipped new thread")
	}
}

func TestA1ChatNativeRootTakesPrecedenceAndTUIEpochRestores(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("tui-response", "turn", 100, 20))
	f.collect()
	tui := f.owners()[0]
	_, err := f.store.UpsertUsageBinding(context.Background(), domain.UsageBindingRecord{SessionID: f.session.ID, Harness: domain.HarnessCodex, NativeRootID: "chat-root", State: domain.UsageBindingActive, UpdatedAt: f.now})
	must(t, err)
	_, err = f.db.Exec("UPDATE sessions SET session_mode='chat',provider_conversation_id='chat-root' WHERE id=?", f.session.ID)
	must(t, err)
	f.write("chat-root", f.meta("chat-root", "", false, f.base))
	raw := strings.ReplaceAll(f.request("chat-response", "turn", 77, 10), `"thread_id":"guardian"`, `"thread_id":"chat-guardian"`)
	f.write("chat-guardian", f.meta("chat-guardian", "chat-root", true, f.base)+raw)
	f.collect()
	chat := f.owners()[0]
	r := f.requests()
	if chat.EpochID == tui.EpochID || len(r) != 1 || r[0].ThreadID != "chat-guardian" {
		t.Fatal("Chat inherited stale TUI root")
	}
	_, err = f.db.Exec("UPDATE sessions SET session_mode='tui' WHERE id=?", f.session.ID)
	must(t, err)
	f.collect()
	if f.owners()[0].EpochID != tui.EpochID || len(f.requests()) != 1 || f.requests()[0].ThreadID != "guardian" {
		t.Fatal("TUI epoch was not durably restored")
	}
}

func TestA4PollingAndModelReplayDoNotRewriteRequestHistory(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20)+f.model("t", "gpt-5.4-mini"))
	f.collect()
	for _, replay := range []bool{false, true} {
		seq, err := f.store.LatestSeq(context.Background())
		must(t, err)
		if replay {
			f.append("guardian", f.model("t", "gpt-5.4-mini"))
		}
		f.collect()
		var count int
		must(t, f.db.QueryRow("SELECT count(*) FROM change_log WHERE seq>?", seq).Scan(&count))
		if count != 0 {
			t.Fatalf("poll/model replay produced %d CDC events, want none", count)
		}
	}
}

func TestA3GuardianHeaderDoesNotRequireSpawnSourceSchema(t *testing.T) {
	for name, source := range map[string]any{"unit-subagent": map[string]any{"subagent": "guardian_review"}, "other-subagent": map[string]any{"subagent": map[string]any{"other": "guardian_review"}}} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.enable(true)
			head := f.line("session_meta", map[string]any{"id": "guardian", "parent_thread_id": "root", "thread_source": "guardian_review", "cli_version": "0.166.0", "source": source}, f.base)
			f.write("guardian", head+f.request("r", "t", 100, 20))
			f.collect()
			if len(f.requests()) != 1 {
				t.Fatal("guardian was parsed as ordinary spawn source")
			}
		})
	}
}

func TestA1A9EnableBoundaryInclusivePurgeFloorExclusive(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	at := f.owners()[0].EnabledAt
	raw := strings.ReplaceAll(f.request("enabled", "t", 100, 20), f.base.Add(2*time.Minute).Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
	f.write("guardian", f.meta("guardian", "root", true, at)+raw)
	f.collect()
	if len(f.requests()) != 1 {
		t.Fatal("enable boundary equality excluded")
	}
	floor := f.now
	must(t, f.store.PurgeGuardian(context.Background(), f.session.ID, floor))
	f.collect()
	equal := strings.ReplaceAll(f.request("purged", "t", 100, 20), f.base.Add(2*time.Minute).Format(time.RFC3339Nano), floor.Format(time.RFC3339Nano))
	f.append("guardian", equal)
	f.collect()
	if len(f.requests()) != 0 {
		t.Fatal("purge boundary equality resurrected")
	}
	f.now = f.now.Add(time.Second)
	fresh := strings.ReplaceAll(f.request("fresh", "t", 100, 20), f.base.Add(2*time.Minute).Format(time.RFC3339Nano), f.now.Format(time.RFC3339Nano))
	f.append("guardian", fresh)
	f.collect()
	if len(f.requests()) != 1 {
		t.Fatal("post-purge request lost")
	}
}

func TestSharedHistoryCollectsOnlyEachOwnersGuardians(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	ctx := context.Background()
	second, err := f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: "project", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.base}, Metadata: domain.SessionMetadata{AgentSessionID: "root-2"}, CreatedAt: f.base, UpdatedAt: f.base})
	must(t, err)
	must(t, f.store.BindSessionAgentAccount(ctx, second.ID, domain.AgentAccountProviderCodex, domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex), f.base.Add(-time.Minute)))
	_, err = f.store.UpsertUsageBinding(ctx, domain.UsageBindingRecord{SessionID: second.ID, Harness: domain.HarnessCodex, NativeRootID: "root-2", State: domain.UsageBindingActive, UpdatedAt: f.base})
	must(t, err)
	f.write("root-2", f.meta("root-2", "", false, f.base))
	f.write("unclaimed", f.meta("unclaimed", "", false, f.base))
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r1", "t1", 100, 20))
	for _, pair := range [][2]string{{"guardian-2", "root-2"}, {"unclaimed-guardian", "unclaimed"}, {"broken-guardian", "missing"}} {
		raw := strings.ReplaceAll(f.request("r2", "t2", 100, 20), `"thread_id":"guardian"`, `"thread_id":"`+pair[0]+`"`)
		f.write(pair[0], f.meta(pair[0], pair[1], true, f.base)+raw)
	}
	for range 3 {
		f.collect()
		for _, o := range f.owners() {
			requests, err := f.store.GuardianRequests(ctx, o.EpochID)
			must(t, err)
			want := "guardian"
			if o.SessionID == second.ID {
				want = "guardian-2"
			}
			if len(requests) != 1 || requests[0].ThreadID != want {
				t.Fatalf("owner %s requests=%+v", o.SessionID, requests)
			}
			codes, err := f.store.GuardianAnomalies(ctx, o.SessionID)
			must(t, err)
			if codes[domain.GuardianOwnerConflict] != 0 || codes[domain.GuardianOwnerUnresolved] != 0 {
				t.Fatalf("unrelated lineage counted for %s: %+v", o.SessionID, codes)
			}
		}
		f.now = f.now.Add(time.Minute)
	}
}

func TestDiscoveryBudgetPrioritizesLatestGuardianAndCachesHeaders(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	old := filepath.Join(f.root, "2025", "01", "01")
	recent := filepath.Join(f.root, f.base.Format("2006/01/02"))
	must(t, os.MkdirAll(old, 0700))
	must(t, os.MkdirAll(recent, 0700))
	for i := range 6000 {
		id := fmt.Sprintf("old-%04d", i)
		path := filepath.Join(old, "rollout-"+id+".jsonl")
		must(t, os.WriteFile(path, []byte(f.meta(id, "", false, f.base.Add(-400*24*time.Hour))), 0600))
		// A copied old archive can have a newer mtime than today's rollout.
		must(t, os.Chtimes(path, f.now, f.now))
	}
	must(t, os.WriteFile(filepath.Join(recent, "rollout-guardian.jsonl"), []byte(f.meta("guardian", "root", true, f.base)+f.request("latest", "t", 100, 20)), 0600))
	f.collect()
	if len(f.requests()) != 1 {
		t.Fatal("old history exhausted budget before latest guardian")
	}
	if f.codes()[domain.GuardianCoverageGap] == 0 {
		t.Fatal("cold partial inventory claimed complete coverage")
	}
	f.collect()
	if len(f.svc.discovery) != 1 {
		t.Fatal("unexpected discovery root groups")
	}
	for _, d := range f.svc.discovery {
		result := d.scan(context.Background(), []string{f.root}, 4096, []string{"root"}, f.now)
		if result.gap || result.reads != 0 || len(result.index) != 6002 {
			t.Fatalf("warm inventory: gap=%v reads=%d entries=%d", result.gap, result.reads, len(result.index))
		}
	}
}

func TestResponseReplayAndNoopWritesEmitNoCDC(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	raw := f.request("r", "t", 100, 20)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+raw+raw+f.model("t", "gpt-5.4-mini"))
	f.collect()
	seq, err := f.store.LatestSeq(context.Background())
	must(t, err)
	f.now = f.now.Add(time.Minute)
	f.append("guardian", raw+raw)
	f.collect()
	if len(f.requests()) != 1 {
		t.Fatal("response replay duplicated facts")
	}
	latest, err := f.store.LatestSeq(context.Background())
	must(t, err)
	if latest != seq {
		t.Fatal("response replay emitted CDC")
	}
	_, err = f.db.Exec("UPDATE guardian_requests SET conflict=conflict")
	must(t, err)
	_, err = f.db.Exec("UPDATE guardian_threads SET last_seen_at=last_seen_at")
	must(t, err)
	latest, err = f.store.LatestSeq(context.Background())
	must(t, err)
	if latest != seq {
		t.Fatal("unchanged raw UPDATE emitted CDC")
	}
}

func TestLineageAnomaliesRequireEvidenceForThisOwner(t *testing.T) {
	for _, tt := range []struct {
		name, parent     string
		nodes            map[string]artifact
		conflicts, known map[string]bool
		claims           map[string]int
		belongs          bool
		code             domain.GuardianCode
	}{
		{name: "own", parent: "root", nodes: map[string]artifact{"root": {meta: header{ID: "root"}}}, belongs: true},
		{name: "other-owned", parent: "other", nodes: map[string]artifact{"other": {meta: header{ID: "other"}}}, claims: map[string]int{"other": 1}},
		{name: "unclaimed", parent: "other", nodes: map[string]artifact{"other": {meta: header{ID: "other"}}}},
		{name: "broken", parent: "absent"},
		{name: "unknown-cycle", parent: "guardian"},
		{name: "known-cycle", parent: "guardian", known: map[string]bool{"guardian": true}, belongs: true, code: domain.GuardianOwnerConflict},
		{name: "own-root-has-parent", parent: "root", nodes: map[string]artifact{"root": {meta: header{ID: "root", Parent: "other"}}}, belongs: true, code: domain.GuardianOwnerConflict},
		{name: "duplicate-own-header", parent: "root", nodes: map[string]artifact{"root": {meta: header{ID: "root"}}}, conflicts: map[string]bool{"guardian": true}, belongs: true, code: domain.GuardianOwnerConflict},
		{name: "crossed-claimed-root", parent: "other", nodes: map[string]artifact{"other": {meta: header{ID: "other", Parent: "root"}}, "root": {meta: header{ID: "root"}}}, claims: map[string]int{"other": 1}, belongs: true, code: domain.GuardianOwnerConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := header{ID: "guardian", Parent: tt.parent}
			if tt.nodes == nil {
				tt.nodes = map[string]artifact{}
			}
			tt.nodes[h.ID] = artifact{meta: h}
			belongs, code := ownerCode(h, "root", tt.nodes, tt.conflicts, tt.claims, tt.known)
			if belongs != tt.belongs || code != tt.code {
				t.Fatalf("belongs=%v code=%s", belongs, code)
			}
		})
	}
}

func TestCollectSharesOneHistorySnapshotAcrossOwners(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	ctx := context.Background()
	second, err := f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: "project", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: f.base}, Metadata: domain.SessionMetadata{AgentSessionID: "root-2"}, CreatedAt: f.base, UpdatedAt: f.base})
	must(t, err)
	must(t, f.store.BindSessionAgentAccount(ctx, second.ID, domain.AgentAccountProviderCodex, domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex), f.base.Add(-time.Minute)))
	_, err = f.store.UpsertUsageBinding(ctx, domain.UsageBindingRecord{SessionID: second.ID, Harness: domain.HarnessCodex, NativeRootID: "root-2", State: domain.UsageBindingActive, UpdatedAt: f.base})
	must(t, err)
	f.write("root-2", f.meta("root-2", "", false, f.base))
	f.write("guardian", f.meta("guardian", "root", true, f.base))
	f.write("guardian-2", f.meta("guardian-2", "root-2", true, f.base)+strings.ReplaceAll(f.request("r2", "t", 100, 20), `"thread_id":"guardian"`, `"thread_id":"guardian-2"`))
	calls := 0
	f.svc.cfg.Roots = func(context.Context, domain.GuardianOwner) ([]string, error) {
		calls++
		if calls == 2 {
			f.append("guardian", f.request("r1", "t", 100, 20))
		}
		return []string{f.root}, nil
	}
	f.collect()
	for _, o := range f.owners() {
		requests, err := f.store.GuardianRequests(ctx, o.EpochID)
		must(t, err)
		if len(requests) != 1 {
			t.Fatalf("owner %s did not see shared snapshot", o.SessionID)
		}
	}
	f.enable(false)
	f.collect()
	if len(f.svc.discovery) != 0 {
		t.Fatal("disabled collection retained header inventory")
	}
}
