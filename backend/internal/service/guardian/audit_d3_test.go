package guardian

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestD3GrowthDoesNotHidePrefixUsage(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	hidden := f.request("hidden", "t", 100, 20)
	filler := `{"type":"unrelated","payload":{}}`
	filler += strings.Repeat(" ", len(hidden)-len(filler)-1) + "\n"
	end := f.request("end", "t", 200, 20)
	f.write("guardian", head+filler+end)
	f.collect()
	f.write("guardian", head+hidden+end+f.request("tail", "t", 100, 20))
	f.collect()
	for range 3 {
		f.now = f.now.Add(time.Minute)
		f.collect()
	}
	t.Logf("requests=%+v codes=%v", f.requests(), f.codes())
	if len(f.requests()) != 3 {
		t.Fatal("growth shortcut silently lost new response inside the prefix")
	}
}
func TestD3ExactByteRevalidationEndHasNoGap(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	a, b := f.request("a", "t", 100, 20), f.request("b", "t", 100, 20)
	remaining := 4*maxLine - len(a) - len(b)
	var noise strings.Builder
	lines := 2
	for remaining > 0 {
		n := min(maxLine, remaining)
		raw := `{"type":"unrelated","payload":{}}`
		if n < len(raw)+1 {
			t.Fatal("invalid padding fixture")
		}
		noise.WriteString(raw + strings.Repeat(" ", n-len(raw)-1) + "\n")
		remaining -= n
		lines++
	}
	data := head + a + noise.String() + b
	if len(data)-len(head) != 4*maxLine {
		t.Fatal("invalid byte fence")
	}
	f.svc.cfg.RecordBudget = lines
	f.write("guardian", data)
	f.collect()
	if f.codes()[domain.GuardianCoverageGap] != 0 {
		t.Fatal("initial fixture already has gap")
	}
	info, err := os.Stat(filepath.Join(f.root, "guardian.jsonl"))
	must(t, err)
	must(t, os.Chtimes(filepath.Join(f.root, "guardian.jsonl"), info.ModTime(), info.ModTime().Add(time.Second)))
	f.svc.cfg.RecordBudget = 512
	f.collect()
	t.Logf("records=%d codes=%v", len(f.requests()), f.codes())
	if f.codes()[domain.GuardianCoverageGap] != 0 {
		t.Fatal("fully revalidated byte-boundary EOF mislabeled as coverage gap")
	}
}
func TestD3ReplayProgressIsAtomicAcrossFailedBatch(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("a", "t", 100, 20)+f.request("b", "t", 100, 20)+f.request("c", "t", 100, 20))
	f.collect()
	path := filepath.Join(f.root, "guardian.jsonl")
	info, err := os.Stat(path)
	must(t, err)
	must(t, os.Chtimes(path, info.ModTime(), info.ModTime().Add(time.Second)))
	f.svc.cfg.RecordBudget = 1
	f.collect()
	owner := f.owners()[0]
	before, err := f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
	must(t, err)
	if before.ReplayUntil <= before.Offset {
		t.Fatal("missing pending interval")
	}
	_, err = f.db.Exec(`CREATE TRIGGER fail_replay BEFORE UPDATE ON guardian_cursors BEGIN SELECT RAISE(ABORT,'synthetic replay failure'); END`)
	must(t, err)
	if err = f.svc.Collect(context.Background()); err == nil {
		t.Fatal("expected transaction failure")
	}
	after, err := f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
	must(t, err)
	if after.Offset != before.Offset || after.ReplayUntil != before.ReplayUntil || after.Generation != before.Generation {
		t.Fatal("failed batch advanced replay cursor")
	}
	_, err = f.db.Exec("DROP TRIGGER fail_replay")
	must(t, err)
	f.svc = New(f.store, f.svc.cfg)
	for range 3 {
		f.collect()
	}
	after, err = f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
	must(t, err)
	if after.ReplayUntil != 0 || len(f.requests()) != 3 {
		t.Fatal("restart lost or duplicated replay facts")
	}
}
func TestD3CheckpointMemoryDropsExpiredCursors(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("a", "t", 100, 20))
	f.collect()
	f.collect()
	if len(f.svc.checks) != 1 {
		t.Fatal("missing retained checkpoint cache")
	}
	f.now = f.base.Add(31 * 24 * time.Hour)
	f.collect()
	if len(f.svc.checks) != 0 {
		t.Fatal("expired native IDs retained in checkpoint memory")
	}
}

func TestD3CheckpointMemoryDropsExpiredCursorsWhenRootsUnavailable(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("a", "t", 100, 20))
	f.collect()
	f.collect()
	if len(f.svc.checks) != 1 {
		t.Fatal("missing checkpoint fixture")
	}
	f.svc.cfg.Roots = func(context.Context, domain.GuardianOwner) ([]string, error) {
		return nil, errors.New("synthetic root unavailable")
	}
	f.now = f.base.Add(29 * 24 * time.Hour)
	f.collect()
	f.now = f.base.Add(32 * 24 * time.Hour)
	f.collect()
	t.Logf("durable_cursors=%d cached_checkpoints=%d", f.count("guardian_cursors"), len(f.svc.checks))
	if f.count("guardian_cursors") != 0 {
		t.Fatal("fixture cursor did not expire")
	}
	if len(f.svc.checks) != 0 {
		t.Fatal("unavailable roots retained native IDs after cursor expiry")
	}
}
