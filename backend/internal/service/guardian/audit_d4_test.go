package guardian

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestD4GrowthDuringVerificationChangesLineBoundary(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base) + f.request("known", "t", 100, 20)
	noise := f.line("unrelated", map[string]string{"padding": strings.Repeat("x", 17)}, f.base)
	f.write("guardian", head+strings.Repeat(noise, 8))
	f.collect()
	f.svc.cfg.RecordBudget = 3
	f.append("guardian", noise)
	f.collect()
	hidden := f.request("hidden", "t", 100, 20)
	if (len(hidden)-len(noise))%len(noise) == 0 {
		hidden = strings.TrimSuffix(hidden, "\n") + " \n"
	}
	// Replace an already visited noise row with a longer response, then grow
	// the tail. The old numeric verification end now falls inside a valid row.
	f.write("guardian", head+hidden+strings.Repeat(noise, 9))
	for i := range 24 {
		f.now = f.now.Add(time.Minute)
		if i == 12 {
			f.svc = New(f.store, f.svc.cfg)
		}
		f.collect()
	}
	c, err := f.store.GuardianCursor(context.Background(), f.owners()[0].EpochID, "guardian")
	must(t, err)
	t.Logf("requests=%d cursor=%+v anomalies=%v", len(f.requests()), c, f.codes())
	if len(f.requests()) != 2 || c.ReplayUntil != 0 {
		t.Fatal("verification failed to recover rewritten response and finish after stable source + restart")
	}
}

func TestD4CheckpointRefreshAtomicAndSilent(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("known", "t", 100, 20))
	f.collect()
	ctx := context.Background()
	o := f.owners()[0]
	c, err := f.store.GuardianCursor(ctx, o.EpochID, "guardian")
	must(t, err)
	p, err := f.store.GuardianPosition(ctx, o.EpochID, c.FileIdentity)
	must(t, err)
	facts := f.requests()
	r := facts[0]
	checkpoint := func() [2]int64 {
		var v [2]int64
		must(t, f.db.QueryRow("SELECT checkpoint_generation,checkpoint_offset FROM guardian_requests WHERE epoch_id=? AND response_id='known'", o.EpochID).Scan(&v[0], &v[1]))
		return v
	}
	before := checkpoint()
	cdc := f.count("change_log")
	r.SourceGeneration++
	r.SourceOffset += 100
	next := c
	next.Generation++
	next.Offset += 100
	next.SourceSize += 100
	next.UpdatedAt = next.UpdatedAt.Add(time.Minute)
	b := domain.GuardianBatch{Owner: o, Thread: domain.GuardianThread{ThreadID: "guardian", ParentID: "root", CLIVersion: "0.166.0", ProviderAt: f.base}, Expected: c, ExpectedPosition: p, Cursor: next, Requests: []domain.GuardianRequest{r}}
	_, err = f.db.Exec(`CREATE TRIGGER fail_checkpoint BEFORE UPDATE ON guardian_cursors BEGIN SELECT RAISE(ABORT,'synthetic checkpoint failure'); END`)
	must(t, err)
	if err = f.store.ApplyGuardianBatch(ctx, b); err == nil {
		t.Fatal("missing injected abort")
	}
	if checkpoint() != before || !reflect.DeepEqual(facts, f.requests()) || f.count("change_log") != cdc {
		t.Fatal("aborted batch changed checkpoint, origin facts, or CDC")
	}
	_, err = f.db.Exec("DROP TRIGGER fail_checkpoint")
	must(t, err)
	must(t, f.store.ApplyGuardianBatch(ctx, b))
	if checkpoint() != [2]int64{r.SourceGeneration, r.SourceOffset} || !reflect.DeepEqual(facts, f.requests()) || f.count("change_log") != cdc {
		t.Fatal("checkpoint refresh changed immutable facts or emitted CDC")
	}
	proof, err := f.store.GuardianContinuityRequests(ctx, next, next.Offset, nil)
	must(t, err)
	if len(proof) != 1 || proof[0].SourceGeneration != r.SourceGeneration || proof[0].SourceOffset != r.SourceOffset {
		t.Fatal("new generation cannot read refreshed checkpoint")
	}
	if err = f.store.ApplyGuardianBatch(ctx, b); !errors.Is(err, domain.ErrGuardianCursorConflict) {
		t.Fatalf("stale batch CAS=%v", err)
	}
	p, err = f.store.GuardianPosition(ctx, o.EpochID, c.FileIdentity)
	must(t, err)
	b.Expected, b.ExpectedPosition = next, p
	b.Cursor.Offset++
	b.Cursor.SourceSize++
	b.Cursor.UpdatedAt = b.Cursor.UpdatedAt.Add(time.Minute)
	input := int64(999)
	r.Tokens.Input = &input
	r.SourceOffset++
	b.Requests = []domain.GuardianRequest{r}
	must(t, f.store.ApplyGuardianBatch(ctx, b))
	if checkpoint() != [2]int64{r.SourceGeneration, r.SourceOffset - 1} || !f.requests()[0].Conflict || f.count("change_log") <= cdc {
		t.Fatal("conflict moved proof or CDC guard suppressed a real change")
	}
	f.now = f.base.Add(31 * 24 * time.Hour)
	must(t, f.store.CleanupGuardian(ctx, f.now))
	if f.count("guardian_requests") != 0 {
		t.Fatal("checkpoint refresh renewed request retention")
	}
}

func TestD4GrowthBoundaryOnlyRewriteCompletesWithoutFalseTruncation(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base) + f.request("known", "t", 100, 20)
	noise := f.line("unrelated", map[string]string{"padding": strings.Repeat("x", 17)}, f.base)
	wider := f.line("unrelated", map[string]string{"padding": strings.Repeat("x", 34)}, f.base)
	f.write("guardian", head+strings.Repeat(noise, 8))
	f.collect()
	f.svc.cfg.RecordBudget = 3
	f.append("guardian", noise)
	f.collect()
	f.write("guardian", head+wider+strings.Repeat(noise, 9))
	for i := range 24 {
		f.now = f.now.Add(time.Minute)
		if i == 12 {
			f.svc = New(f.store, f.svc.cfg)
		}
		f.collect()
	}
	c, err := f.store.GuardianCursor(context.Background(), f.owners()[0].EpochID, "guardian")
	must(t, err)
	t.Logf("requests=%d cursor=%+v anomalies=%v", len(f.requests()), c, f.codes())
	if len(f.requests()) != 1 || c.ReplayUntil != 0 || f.codes()[domain.GuardianSourceTruncated] != 0 || f.codes()[domain.GuardianCoverageGap] != 0 {
		t.Fatal("a valid complete source with unchanged request evidence has stuck replay or false truncation/gap")
	}
}
