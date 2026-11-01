package guardian

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestRewriteDuringGrowthPreservesRecordFraming(t *testing.T) {
	for _, usage := range []bool{false, true} {
		for _, delta := range []int{-128, 0, 128} {
			for _, row := range []int{0, 1, 6} {
				t.Run(fmt.Sprintf("usage_%t/delta_%d/row_%d", usage, delta, row), func(t *testing.T) {
					f := newFixture(t)
					f.enable(true)
					head := f.meta("guardian", "root", true, f.base) + f.request("known", "t", 100, 20)
					noise := f.line("unrelated", map[string]string{"padding": strings.Repeat("x", 1024)}, f.base)
					f.write("guardian", head+strings.Repeat(noise, 8))
					f.collect()
					f.svc.cfg.RecordBudget = 3
					f.append("guardian", noise)
					f.collect()
					owner := f.owners()[0]
					before, err := f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
					must(t, err)
					if before.ReplayOffset == 0 || before.ReplayUntil == 0 {
						t.Fatal("missing pending verification fixture")
					}
					replacement := f.line("unrelated", map[string]string{"padding": strings.Repeat("y", 1024+delta)}, f.base)
					if usage {
						r := f.request("hidden", "t", 100, 20)
						replacement = strings.TrimSuffix(r, "\n") + strings.Repeat(" ", len(noise)+delta-len(r)) + "\n"
					}
					data := head + strings.Repeat(noise, row) + replacement + strings.Repeat(noise, 9-row)
					atBoundary := data[before.ReplayUntil-1] == '\n'
					if atBoundary != (delta == 0) {
						t.Fatal("fixture did not move the saved byte boundary as intended")
					}
					f.write("guardian", data)
					for i := range 24 {
						f.now = f.now.Add(time.Minute)
						if i == 1 || i == 12 {
							f.svc = New(f.store, f.svc.cfg)
						}
						f.collect()
					}
					c, err := f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
					must(t, err)
					want := 1
					if usage {
						want++
					}
					if len(f.requests()) != want || c.ReplayUntil != 0 || c.ReplayOffset != 0 || c.ReplayAgain {
						t.Fatalf("requests=%d want=%d; replay=%d..%d again=%t", len(f.requests()), want, c.ReplayOffset, c.ReplayUntil, c.ReplayAgain)
					}
					if f.codes()[domain.GuardianSourceTruncated] != 0 || !usage && f.codes()[domain.GuardianCoverageGap] != 0 {
						t.Fatalf("valid complete source received false anomaly: %v", f.codes())
					}
					spy := &idleBatchSpy{GuardianStore: f.store}
					f.svc.store = spy
					cdc := f.count("change_log")
					for range 3 {
						f.now = f.now.Add(time.Minute)
						f.collect()
					}
					if spy.calls != 0 || f.count("change_log") != cdc {
						t.Fatal("completed verification did not return to idle polling")
					}
				})
			}
		}
	}
}

func TestGrowthRewriteDoesNotParseSuffixAsRecord(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base) + f.request("known", "t", 100, 20)
	noise := f.line("unrelated", map[string]string{"padding": strings.Repeat("x", 128)}, f.base)
	f.write("guardian", head+strings.Repeat(noise, 8))
	f.collect()
	f.svc.cfg.RecordBudget = 3
	f.append("guardian", noise)
	f.collect()
	c, err := f.store.GuardianCursor(context.Background(), f.owners()[0].EpochID, "guardian")
	must(t, err)
	f.write("guardian", head+strings.Repeat("x", int(c.Offset)-len(head))+f.request("fragment", "t", 100, 20)+noise)
	for i := range 24 {
		if i == 1 {
			f.svc = New(f.store, f.svc.cfg)
		}
		f.collect()
	}
	c, err = f.store.GuardianCursor(context.Background(), f.owners()[0].EpochID, "guardian")
	must(t, err)
	if len(f.requests()) != 1 || c.ReplayUntil != 0 || f.codes()[domain.GuardianInvalidMetadata] == 0 {
		t.Fatalf("a line suffix became a standalone response: requests=%d replay=%d codes=%v", len(f.requests()), c.ReplayUntil, f.codes())
	}
}

func TestVerificationSoftEndWaitsForCompleteLine(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base) + f.request("known", "t", 100, 20)
	noise := f.line("unrelated", map[string]string{"padding": strings.Repeat("x", 17)}, f.base)
	f.write("guardian", head+strings.Repeat(noise, 8))
	f.collect()
	f.svc.cfg.RecordBudget = 3
	f.append("guardian", noise)
	f.collect()
	partial := strings.TrimSuffix(f.request("hidden", "t", 100, 20), "\n") + strings.Repeat(" ", 2*len(head+strings.Repeat(noise, 9)))
	f.write("guardian", head+partial)
	for range 4 {
		f.collect()
	}
	if len(f.requests()) != 1 || f.codes()[domain.GuardianPartialLine] == 0 || f.codes()[domain.GuardianSourceTruncated] != 0 || f.codes()[domain.GuardianCoverageGap] != 0 {
		t.Fatalf("incomplete line was parsed or mislabeled: requests=%d codes=%v", len(f.requests()), f.codes())
	}
	f.svc = New(f.store, f.svc.cfg)
	f.append("guardian", "\n")
	for range 12 {
		f.collect()
	}
	c, err := f.store.GuardianCursor(context.Background(), f.owners()[0].EpochID, "guardian")
	must(t, err)
	if len(f.requests()) != 2 || c.ReplayUntil != 0 || f.codes()[domain.GuardianSourceTruncated] != 0 {
		t.Fatal("completed line did not resume and finish verification after restart")
	}
}

func TestEnableFenceInsideLineRejectsContinuation(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base.Add(-time.Hour))+"unread old prefix ")
	f.collect()
	f.append("guardian", f.request("fragment", "t", 100, 20)+f.request("after", "t", 100, 20))
	f.collect()
	if got := f.requests(); len(got) != 1 || got[0].ResponseID != "after" {
		t.Fatalf("enable fence accepted an old line continuation: requests=%d", len(got))
	}
	// A later replay must retain the original floor and skip its partial line.
	f.append("guardian", f.line("unrelated", map[string]string{}, f.base))
	f.collect()
	f.svc = New(f.store, f.svc.cfg)
	f.collect()
	if got := f.requests(); len(got) != 1 || got[0].ResponseID != "after" {
		t.Fatal("verification crossed the original enable floor")
	}
}
