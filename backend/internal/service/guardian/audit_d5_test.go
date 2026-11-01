package guardian

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestD5RewriteAtDiscardingCursorDoesNotLoseNewBoundaryRecord(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base) + f.request("known", "t", 100, 20)
	noise := f.line("unrelated", map[string]string{}, f.base)
	f.svc.cfg.RecordBudget = 2
	f.write("guardian", head+strings.Repeat("x", maxLine+1)+"\n"+noise)
	f.collect()
	o := f.owners()[0]
	before, err := f.store.GuardianCursor(context.Background(), o.EpochID, "guardian")
	must(t, err)
	if !before.Discarding || before.Offset != int64(len(head)+maxLine+1) {
		t.Fatal("missing oversize continuation fixture")
	}
	// Move the newline one byte left: the saved cursor is now exactly at the
	// beginning of a new complete response, not inside the discarded line.
	f.write("guardian", head+strings.Repeat("x", maxLine)+"\n"+f.request("new-boundary", "t", 100, 20)+strings.Repeat(noise, 3))
	f.svc.cfg.RecordBudget = 16
	f.svc = New(f.store, f.svc.cfg)
	for range 12 {
		f.now = f.now.Add(time.Minute)
		f.collect()
	}
	after, err := f.store.GuardianCursor(context.Background(), o.EpochID, "guardian")
	must(t, err)
	t.Logf("requests=%d discarding=%t replay=%d..%d again=%t codes=%v", len(f.requests()), after.Discarding, after.ReplayOffset, after.ReplayUntil, after.ReplayAgain, f.codes())
	if len(f.requests()) != 2 || after.ReplayUntil != 0 {
		t.Fatal("persisted discarding skipped a valid new response at the rewritten boundary")
	}
}

func TestD5FloorSurvivesRewriteAndRestart(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base.Add(-time.Hour))
	f.write("guardian", head+strings.Repeat("x", 1024))
	f.collect()
	f.append("guardian", f.request("fragment", "t", 100, 20)+f.request("after", "t", 100, 20))
	f.collect()
	if rows := f.requests(); len(rows) != 1 || rows[0].ResponseID != "after" {
		t.Fatal("initial floor accepted a partial-line suffix")
	}
	o := f.owners()[0]
	before, err := f.store.GuardianCursor(context.Background(), o.EpochID, "guardian")
	must(t, err)
	forbidden := f.request("before-floor", "t", 100, 20)
	prefix := head + forbidden + strings.Repeat("x", int(before.ReadStartOffset)-len(head)-len(forbidden))
	f.write("guardian", prefix+f.request("fragment", "t", 100, 20)+f.request("after", "t", 100, 20)+f.request("tail", "t", 100, 20))
	f.svc.cfg.RecordBudget = 1
	for range 16 {
		f.svc = New(f.store, f.svc.cfg)
		f.now = f.now.Add(time.Minute)
		f.collect()
	}
	after, err := f.store.GuardianCursor(context.Background(), o.EpochID, "guardian")
	must(t, err)
	if rows := f.requests(); len(rows) != 2 || rows[0].ResponseID != "after" || rows[1].ResponseID != "tail" || after.ReadStartOffset != before.ReadStartOffset || after.ReplayUntil != 0 {
		t.Fatalf("floor crossed or replay not completed: requests=%d floor=%d want=%d replay=%d", len(rows), after.ReadStartOffset, before.ReadStartOffset, after.ReplayUntil)
	}
}

func TestD5AppendOnlyDiscardingDoesNotParseContinuation(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base) + f.request("known", "t", 100, 20)
	f.svc.cfg.RecordBudget = 2
	f.write("guardian", head+strings.Repeat("x", maxLine+1))
	f.collect()
	c, err := f.store.GuardianCursor(context.Background(), f.owners()[0].EpochID, "guardian")
	must(t, err)
	if !c.Discarding {
		t.Fatal("missing pending oversize discard")
	}
	f.append("guardian", f.request("fragment", "t", 100, 20)+f.request("after", "t", 100, 20))
	f.svc.cfg.RecordBudget = 16
	f.svc = New(f.store, f.svc.cfg)
	for range 8 {
		f.collect()
	}
	rows := f.requests()
	if len(rows) != 2 || rows[0].ResponseID != "after" || rows[1].ResponseID != "known" {
		t.Fatal("an append-only continuation became a standalone response")
	}
}

func TestD5ReplayDiscardingRechecksNewBoundary(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base) + f.request("known", "t", 100, 20)
	noise := f.line("unrelated", map[string]string{}, f.base)
	f.svc.cfg.RecordBudget = 16
	f.write("guardian", head+strings.Repeat("x", 2*(maxLine+1))+"\n"+noise)
	f.collect()
	f.svc.cfg.RecordBudget = 1
	f.append("guardian", noise)
	f.collect()
	f.collect()
	f.collect()
	owner := f.owners()[0]
	before, err := f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
	must(t, err)
	if !before.ReplayDiscarding || before.ReplayOffset != int64(len(head)+maxLine+1) {
		t.Fatal("missing background oversize continuation fixture")
	}
	f.write("guardian", head+strings.Repeat("x", maxLine)+"\n"+f.request("replay-boundary", "t", 100, 20)+strings.Repeat("x", maxLine+1)+"\n"+strings.Repeat(noise, 3))
	f.svc.cfg.RecordBudget = 16
	f.svc = New(f.store, f.svc.cfg)
	f.collect()
	if len(f.requests()) != 2 {
		t.Fatal("stale background discarding skipped a complete boundary response")
	}
	for range 12 {
		f.collect()
	}
	after, err := f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
	must(t, err)
	if after.ReplayUntil != 0 || len(f.requests()) != 2 {
		t.Fatal("stable oversize verification did not finish after refreshing framing")
	}
}
