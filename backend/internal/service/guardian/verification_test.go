package guardian

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestGrowthVerificationRevisitsChangesAndSurvivesFailedBatch(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.cfg.RecordBudget = 4096
	source := largeIdleSource(f)
	f.write("guardian", source)
	f.collect()
	f.svc.cfg.RecordBudget = 512
	tail := f.request("tail-1", "t", 100, 20)
	f.append("guardian", tail)
	f.collect()
	if len(f.requests()) != 3 {
		t.Fatal("prefix verification delayed forward tail ingestion")
	}
	noise := f.line("unrelated", map[string]string{"padding": strings.Repeat("x", 1800)}, f.base)
	offset := strings.Index(source, noise) + 10*len(noise)
	hidden := f.request("hidden", "t", 100, 20)
	hidden = strings.TrimSuffix(hidden, "\n") + strings.Repeat(" ", len(noise)-len(hidden)) + "\n"
	f.write("guardian", source[:offset]+hidden+source[offset+len(noise):]+tail+f.request("tail-2", "t", 100, 20))
	f.collect()
	if len(f.requests()) != 4 {
		t.Fatal("new tail was not collected while prefix verification remained pending")
	}
	owner := f.owners()[0]
	before, err := f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
	must(t, err)
	_, err = f.db.Exec(`CREATE TRIGGER fail_growth_verification BEFORE UPDATE ON guardian_cursors BEGIN SELECT RAISE(ABORT,'synthetic verification failure'); END`)
	must(t, err)
	if err := f.svc.Collect(context.Background()); err == nil {
		t.Fatal("missing injected transaction failure")
	}
	after, err := f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed transaction advanced durable verification state")
	}
	_, err = f.db.Exec("DROP TRIGGER fail_growth_verification")
	must(t, err)
	f.svc = New(f.store, f.svc.cfg)
	for range 10 {
		f.now = f.now.Add(time.Minute)
		f.collect()
	}
	after, err = f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
	must(t, err)
	if len(f.requests()) != 5 || after.ReplayUntil != 0 || f.codes()[domain.GuardianCoverageGap] == 0 {
		t.Fatal("pending verification did not revisit the changed prefix and finish after restart")
	}
	spy := &idleBatchSpy{GuardianStore: f.store}
	f.svc.store = spy
	cdc := f.count("change_log")
	for range 3 {
		f.now = f.now.Add(time.Minute)
		f.collect()
	}
	if spy.calls != 0 || f.count("change_log") != cdc {
		t.Fatal("completed growth verification kept replaying an idle source")
	}
}

func TestGrowthVerificationPreservesOversizeDiscardingAcrossRestart(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.cfg.RecordBudget = 16
	f.write("guardian", f.meta("guardian", "root", true, f.base)+strings.Repeat(" ", 2*(maxLine+1))+f.request("embedded", "t", 100, 20)+f.request("end", "t", 100, 20))
	f.collect()
	if len(f.requests()) != 1 {
		t.Fatal("oversize line was not excluded initially")
	}
	f.svc.cfg.RecordBudget = 1
	f.append("guardian", f.request("tail", "t", 100, 20))
	f.collect()
	for range 6 {
		f.svc = New(f.store, f.svc.cfg)
		f.now = f.now.Add(time.Minute)
		f.collect()
	}
	c, err := f.store.GuardianCursor(context.Background(), f.owners()[0].EpochID, "guardian")
	must(t, err)
	if len(f.requests()) != 2 || f.codes()[domain.GuardianCoverageGap] != 0 || c.ReplayUntil != 0 {
		t.Fatal("verification parsed a discarded line fragment or invented a budget gap")
	}
}
