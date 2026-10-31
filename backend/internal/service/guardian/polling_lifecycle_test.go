package guardian

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestIdleOwnerPreservesPositionBeyondRetention(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("old", "t", 100, 20))
	f.collect()
	if len(f.requests()) != 1 || len(f.codes()) != 0 {
		t.Fatal("invalid idle source fixture")
	}
	for _, day := range []int{31, 62, 93} {
		f.now = f.base.Add(time.Duration(day) * 24 * time.Hour)
		f.svc = New(f.store, f.svc.cfg)
		f.collect()
		if gap := f.codes()[domain.GuardianCoverageGap]; gap != 0 {
			t.Fatalf("unchanged live source on day %d: coverage_gap=%d", day, gap)
		}
		if f.count("guardian_positions") != 1 || f.count("guardian_requests") != 0 || f.count("guardian_threads") != 0 || f.count("guardian_cursors") != 0 {
			t.Fatal("idle source lost its minimal position or retained expired native IDs")
		}
	}
	_, err := f.db.Exec("UPDATE sessions SET is_terminated=1 WHERE id=?", f.session.ID)
	must(t, err)
	f.now = f.now.Add(time.Hour)
	f.collect()
	if f.count("guardian_positions") != 0 || f.count("guardian_epochs") != 0 {
		t.Fatal("inactive owner retained expired replay state")
	}
}

func TestRevalidatedRequestsRemainCheckpointsAcrossGenerations(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	padding := f.line("unrelated", map[string]string{"padding": strings.Repeat("x", 300)}, f.base)
	f.write("guardian", head+f.request("a", "t", 100, 20)+padding+f.request("b", "t", 100, 20))
	f.collect()
	facts := f.requests()
	path := filepath.Join(f.root, "guardian.jsonl")
	before, err := os.Stat(path)
	must(t, err)
	reordered := head + padding + f.request("a", "t", 100, 20) + f.request("b", "t", 100, 20)
	f.write("guardian", reordered)
	must(t, os.Chtimes(path, before.ModTime().Add(time.Second), before.ModTime().Add(time.Second)))
	f.now = f.now.Add(time.Minute)
	f.collect()
	cursor, err := f.store.GuardianCursor(context.Background(), f.owners()[0].EpochID, "guardian")
	must(t, err)
	if cursor.Generation == 0 || !reflect.DeepEqual(facts, f.requests()) {
		t.Fatal("revalidation did not advance generation while preserving immutable request facts")
	}
	f.svc = New(f.store, f.svc.cfg)
	spy := &idleBatchSpy{GuardianStore: f.store}
	f.svc.store = spy
	cdc := f.count("change_log")
	f.now = f.now.Add(time.Minute)
	f.collect()
	if spy.calls != 0 || f.count("change_log") != cdc {
		t.Fatal("verified request offsets became false rewrite evidence on idle polling")
	}
	before, err = os.Stat(path)
	must(t, err)
	f.write("guardian", strings.Replace(reordered, `"response_id":"a"`, `"response_id":"c"`, 1))
	must(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
	f.now = f.now.Add(time.Minute)
	f.collect()
	if got := len(f.requests()); got != 3 {
		t.Fatalf("generation %d lost retained checkpoint evidence: requests=%d, missing rewritten response c", cursor.Generation, got)
	}
}
