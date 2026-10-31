package guardian

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAuditTruncateAndRegrowBetweenPolls(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	f.write("guardian", head+f.request("old", "t", 100, 20))
	f.collect()
	before, err := os.Stat(filepath.Join(f.root, "guardian.jsonl"))
	must(t, err)
	f.write("guardian", head+f.request("new", "t", 100, 20))
	after, err := os.Stat(filepath.Join(f.root, "guardian.jsonl"))
	must(t, err)
	if !os.SameFile(before, after) || before.Size() != after.Size() {
		t.Fatal("invalid reproduction")
	}
	f.collect()
	t.Logf("before_size=%d after_size=%d requests=%+v codes=%v", before.Size(), after.Size(), f.requests(), f.codes())
	if len(f.requests()) != 2 || f.codes()[domain.GuardianSourceTruncated] == 0 {
		t.Fatal("truncated-and-regrown source silently skipped new response")
	}
}
func TestAuditMissingRegisteredRootReportsCoverage(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	must(t, os.RemoveAll(f.root))
	f.collect()
	if f.codes()[domain.GuardianSourceMissing] == 0 && f.codes()[domain.GuardianCoverageGap] == 0 {
		t.Fatal("missing registered source root silently reports no anomaly")
	}
}

func TestAuditConcurrentCollectors(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20)+f.model("t", "gpt-5.4-mini"))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- New(f.store, f.svc.cfg).Collect(context.Background()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	if len(f.requests()) != 1 || f.requests()[0].ModelID != "gpt-5.4-mini" {
		t.Fatal("concurrent collector lost or duplicated response")
	}
}

type auditCapture struct {
	ports.GuardianStore
	batches []domain.GuardianBatch
}

func (a *auditCapture) ApplyGuardianBatch(ctx context.Context, b domain.GuardianBatch) error {
	a.batches = append(a.batches, b)
	return a.GuardianStore.ApplyGuardianBatch(ctx, b)
}
func TestAuditSensitivePayloadNeverCrossesParserBatch(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	spy := &auditCapture{GuardianStore: f.store}
	f.svc = New(spy, f.svc.cfg)
	const sentinel = "PRIVATE-U16-AUDIT-DO-NOT-STORE"
	var raw map[string]any
	must(t, json.Unmarshal([]byte(f.request("r", "t", 100, 20)), &raw))
	payload := raw["payload"].(map[string]any)
	for _, key := range []string{"command", "rawCommand", "argv", "stdin", "cwd", "files", "path", "host", "url", "mcp", "messages", "rationale", "prompt", "permissions", "creator_user_id", "unknown"} {
		payload[key] = map[string]any{"nested": sentinel}
	}
	encoded, err := json.Marshal(raw)
	must(t, err)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+string(encoded)+"\n"+f.model("t", "gpt-5.4-mini"))
	f.collect()
	if len(spy.batches) != 1 || len(spy.batches[0].Requests) != 1 || len(spy.batches[0].Models) != 1 {
		t.Fatal("missing exercised whitelist batch")
	}
	result, err := json.Marshal(spy.batches)
	must(t, err)
	if strings.Contains(string(result), sentinel) {
		t.Fatal("sensitive payload crossed parser boundary")
	}
}

func TestAuditDuplicateHeadersCannotHideResponseConflict(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	f.write("guardian", head+f.request("same", "t", 100, 20))
	f.write("copy", head+f.request("same", "t", 900, 20))
	f.collect()
	r := f.requests()
	t.Logf("requests=%+v codes=%v", r, f.codes())
	// Independent physical copies are quarantined before either can become fact.
	if len(r) != 0 || (f.codes()[domain.GuardianSourceConflict] == 0 && f.codes()[domain.GuardianOwnerConflict] == 0) {
		t.Fatalf("independent duplicate sources were not quarantined: requests=%v codes=%v", r, f.codes())
	}
}

func TestAuditPersistedCounterBucketsConserve(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	text := f.meta("guardian", "root", true, f.base)
	for i, cached := range []int64{24, 25, 75, 95} {
		text += f.line("event_msg", map[string]any{"type": "token_usage_record", "thread_id": "guardian", "turn_id": "t", "response_id": []string{"r0", "r1", "r2", "r3"}[i], "usage": map[string]any{"input_tokens": 100, "cached_input_tokens": cached, "cache_write_input_tokens": 1, "output_tokens": 30, "reasoning_output_tokens": 5}}, f.base.Add(2*60*1000000000))
	}
	f.write("guardian", text)
	f.collect()
	requests := f.requests()
	if len(requests) != 4 {
		t.Fatal("lost vectors")
	}
	totals := map[string][4]int64{}
	for _, r := range requests {
		v := r.Tokens
		if v.CacheWrite == nil || *v.CacheWrite != 1 || v.Reasoning == nil || *v.Reasoning != 5 {
			t.Fatal("subset counters changed")
		}
		b := v.CacheBucket()
		x := totals[b]
		x[0] += *v.Input
		x[1] += *v.Cached
		x[2] += *v.Input - *v.Cached
		x[3] += *v.Output
		totals[b] = x
	}
	want := map[string][4]int64{"below_25": {100, 24, 76, 30}, "25_to_75": {100, 25, 75, 30}, "75_to_95": {100, 75, 25, 30}, "at_least_95": {100, 95, 5, 30}}
	a, _ := json.Marshal(totals)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		t.Fatalf("persisted bucket sum got %s want %s", a, b)
	}
}

func TestSamePhysicalAliasesRemainCollectible(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20))
	must(t, os.Link(filepath.Join(f.root, "guardian.jsonl"), filepath.Join(f.root, "alias.jsonl")))
	f.collect()
	if len(f.requests()) != 1 || f.codes()[domain.GuardianOwnerConflict] != 0 || f.codes()[domain.GuardianSourceConflict] != 0 {
		t.Fatalf("physical aliases misclassified: %v", f.codes())
	}
}

func TestUnreadableRegisteredSubdirectoryReportsCoverage(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	dir := filepath.Join(f.root, "2026")
	must(t, os.Mkdir(dir, 0700))
	must(t, os.Chmod(dir, 0000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	f.collect()
	if f.codes()[domain.GuardianCoverageGap] == 0 {
		t.Fatal("unreadable registered subtree silently skipped")
	}
}

func TestContinuityRecoveryIsBoundedAndSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	f.write("guardian", head+f.request("old", "t", 100, 20))
	f.collect()
	f.svc = New(f.store, f.svc.cfg)
	f.svc.cfg.RecordBudget = 1
	f.write("guardian", head+f.request("new", "t", 100, 20)+f.request("end", "t", 100, 20))
	f.collect()
	if len(f.requests()) != 2 || f.codes()[domain.GuardianSourceTruncated] == 0 || f.codes()[domain.GuardianCoverageGap] == 0 {
		t.Fatalf("bounded recovery missing: %v %v", f.requests(), f.codes())
	}
	f.collect()
	if len(f.requests()) != 3 {
		t.Fatal("bounded recovery failed to resume")
	}
}

func TestChangedSourceWithoutRetainedEvidenceReportsGap(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	f.write("guardian", head+f.model("t", "gpt-5.4-mini"))
	f.collect()
	f.write("guardian", head+f.model("t", "gpt-5.4-nano"))
	f.collect()
	if f.codes()[domain.GuardianCoverageGap] == 0 {
		t.Fatal("unproven continuity reported as complete")
	}
}

func TestContinuityChecksFactsEvenWhenFileMetadataIsRestored(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	f.write("guardian", head+f.request("old", "t", 100, 20))
	f.collect()
	path := filepath.Join(f.root, "guardian.jsonl")
	before, err := os.Stat(path)
	must(t, err)
	f.write("guardian", head+f.request("new", "t", 100, 20))
	must(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
	f.collect()
	if len(f.requests()) != 2 || f.codes()[domain.GuardianSourceTruncated] == 0 {
		t.Fatal("filesystem metadata replaced structured continuity check")
	}
}
