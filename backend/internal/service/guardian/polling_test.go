package guardian

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestStringSubagentsDoNotHideMissingSources(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20))
	f.collect()
	for _, kind := range []string{"review", "compact"} {
		f.write(kind, f.line("session_meta", map[string]any{"id": kind, "cli_version": "0.166.0", "source": map[string]any{"subagent": kind}}, f.base))
	}
	must(t, os.Remove(filepath.Join(f.root, "guardian.jsonl")))
	f.collect()
	if f.codes()[domain.GuardianSourceMissing] != 1 || f.codes()[domain.GuardianCoverageGap] != 0 {
		t.Fatalf("string subagent invalidated discovery: %v", f.codes())
	}
}

func TestPreviouslyOwnedBrokenLineageIsReported(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20))
	f.collect()
	must(t, os.Remove(filepath.Join(f.root, "root.jsonl")))
	f.collect()
	if f.codes()[domain.GuardianOwnerUnresolved] != 1 {
		t.Fatalf("known owner lost without anomaly: %v", f.codes())
	}
	seq, err := f.store.LatestSeq(context.Background())
	must(t, err)
	f.collect()
	latest, err := f.store.LatestSeq(context.Background())
	must(t, err)
	if latest != seq {
		t.Fatal("repeated broken lineage emitted CDC")
	}
}

func TestPersistentSourceAnomaliesEmitCDCOnlyOnChange(t *testing.T) {
	for _, kind := range []string{"unsupported", "missing", "partial", "conflict", "unavailable-roots"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.enable(true)
			header := f.meta("guardian", "root", true, f.base)
			raw := f.request("r", "t", 100, 20)
			f.write("guardian", header+raw)
			f.collect()
			switch kind {
			case "unsupported":
				f.write("guardian", strings.ReplaceAll(header, "0.166.0", "9.999.0")+raw)
			case "missing":
				must(t, os.Remove(filepath.Join(f.root, "guardian.jsonl")))
			case "partial":
				f.append("guardian", strings.TrimSuffix(f.request("tail", "t", 100, 20), "\n"))
			case "conflict":
				f.write("guardian", strings.ReplaceAll(header, "0.166.0", "0.146.0")+raw)
			case "unavailable-roots":
				must(t, os.RemoveAll(f.root))
			}
			f.collect()
			seq, err := f.store.LatestSeq(context.Background())
			must(t, err)
			before := f.codes()
			// Dedupe must survive collector/daemon restart, not only a process-local map.
			f.svc = New(f.store, f.svc.cfg)
			for range 3 {
				f.now = f.now.Add(time.Minute)
				f.collect()
			}
			latest, err := f.store.LatestSeq(context.Background())
			must(t, err)
			if latest != seq {
				t.Fatalf("unchanged %s emitted CDC: before=%v after=%v", kind, before, f.codes())
			}
		})
	}
}

func TestLargeInventoryStaysWarmBeyondFormerCacheLimit(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	for i := range 12500 {
		id := fmt.Sprintf("old-%05d", i)
		f.write(id, f.meta(id, "", false, f.base.Add(-24*time.Hour)))
	}
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20))
	f.collect()
	seq, err := f.store.LatestSeq(context.Background())
	must(t, err)
	for range 4 {
		f.now = f.now.Add(time.Minute)
		f.collect()
	}
	latest, err := f.store.LatestSeq(context.Background())
	must(t, err)
	if latest != seq {
		t.Fatalf("cache budget churn emitted CDC: %v", f.codes())
	}
	f.now = f.now.Add(2 * time.Hour)
	for _, d := range f.svc.discovery {
		result := d.scan(context.Background(), []string{f.root}, 4096, []string{"root", "guardian"})
		if result.gap || result.reads != 0 || len(result.index) != 12502 {
			t.Fatalf("warm cache expired or evicted: gap=%v reads=%d entries=%d", result.gap, result.reads, len(result.index))
		}
	}
}

func TestObservedVersionFixtures(t *testing.T) {
	for _, version := range []string{"0.146.0", "0.164.0", "0.165.0", "0.166.0"} {
		t.Run(version, func(t *testing.T) {
			f := newFixture(t)
			f.enable(true)
			f.write("guardian", strings.ReplaceAll(f.meta("guardian", "root", true, f.base), "0.166.0", version)+f.request("r", "t", 100, 20))
			f.collect()
			requests := f.requests()
			if len(requests) != 1 || requests[0].CLIVersion != version || f.codes()[domain.GuardianUnsupportedVersion] != 0 {
				t.Fatalf("observed version rejected: %s %v", version, f.codes())
			}
		})
	}
}

func TestAnomalyRecoveryAndSourceChangesCountAgain(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20))
	f.collect()
	path := filepath.Join(f.root, "guardian.jsonl")
	parked := filepath.Join(t.TempDir(), "parked.jsonl")
	for range 2 {
		must(t, os.Rename(path, parked))
		f.collect()
		seq, err := f.store.LatestSeq(context.Background())
		must(t, err)
		f.collect()
		latest, err := f.store.LatestSeq(context.Background())
		must(t, err)
		if latest != seq {
			t.Fatal("stable missing source emitted CDC")
		}
		must(t, os.Rename(parked, path))
		f.collect()
	}
	if f.codes()[domain.GuardianSourceMissing] != 2 {
		t.Fatalf("distinct missing episodes not counted: %v", f.codes())
	}
}

func TestConcurrentPersistentAnomalyIsCountedOnce(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", strings.ReplaceAll(f.meta("guardian", "root", true, f.base), "0.166.0", "9.999.0"))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- New(f.store, f.svc.cfg).Collect(context.Background()) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	if f.codes()[domain.GuardianUnsupportedVersion] != 1 {
		t.Fatalf("concurrent observations counted repeatedly: %v", f.codes())
	}
	var nativeIDs int
	must(t, f.db.QueryRow("SELECT count(*) FROM guardian_anomaly_states WHERE file_identity LIKE '%guardian%' OR file_identity LIKE '%root%'").Scan(&nativeIDs))
	if nativeIDs != 0 {
		t.Fatal("native ID persisted in anomaly state")
	}
	must(t, f.store.PurgeGuardian(context.Background(), f.session.ID, f.now))
	if f.count("guardian_anomaly_states") != 0 {
		t.Fatal("purge left observation state")
	}
}

func TestAnomalyDedupeExpiresWithItsCounterWindow(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20))
	f.write("bad-old", strings.ReplaceAll(f.meta("bad-old", "root", true, f.base), "0.166.0", "9.999.0"))
	f.collect()
	f.now = f.now.Add(29 * 24 * time.Hour)
	f.write("bad-new", strings.ReplaceAll(f.meta("bad-new", "root", true, f.now.Add(-time.Minute)), "0.166.0", "9.999.0"))
	f.collect()
	must(t, os.Remove(filepath.Join(f.root, "bad-old.jsonl")))
	f.now = f.now.Add(2 * 24 * time.Hour)
	f.collect()
	if f.codes()[domain.GuardianUnsupportedVersion] != 1 {
		t.Fatalf("expired counter hid active newer source: %v", f.codes())
	}
}

func TestAlreadyConflictedResponsesRemainSilentOnIdlePoll(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("r", "t", 100, 20)+f.request("r", "t", 900, 20))
	f.collect()
	seq, err := f.store.LatestSeq(context.Background())
	must(t, err)
	f.now = f.now.Add(time.Minute)
	f.collect()
	latest, err := f.store.LatestSeq(context.Background())
	must(t, err)
	if latest != seq || !f.requests()[0].Conflict {
		t.Fatalf("idle conflict changed observable history: %v", f.codes())
	}
}

func TestDisableBetweenDiscoveryAndAnomalyCommit(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", strings.ReplaceAll(f.meta("guardian", "root", true, f.base), "0.166.0", "9.999.0"))
	f.svc.cfg.Roots = func(context.Context, domain.GuardianOwner) ([]string, error) {
		f.enable(false)
		return []string{f.root}, nil
	}
	f.collect()
	if f.count("guardian_anomalies") != 0 || f.count("guardian_anomaly_states") != 0 {
		t.Fatal("disabled policy accepted new anomaly metadata")
	}
}
