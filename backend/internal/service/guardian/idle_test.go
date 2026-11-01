package guardian

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type idleBatchSpy struct {
	ports.GuardianStore
	calls int
}

func (s *idleBatchSpy) ApplyGuardianBatch(ctx context.Context, b domain.GuardianBatch) error {
	s.calls++
	return s.GuardianStore.ApplyGuardianBatch(ctx, b)
}

func largeIdleSource(f *fixture) string {
	return f.meta("guardian", "root", true, f.base) + f.request("a", "t", 100, 20) + strings.Repeat(f.line("unrelated", map[string]string{"padding": strings.Repeat("x", 1800)}, f.base), 1200) + f.request("b", "t", 100, 20)
}

func TestIdlePollDoesNotReplayBatchOrWrite(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.cfg.RecordBudget = 4096
	f.write("guardian", largeIdleSource(f))
	f.collect()
	f.svc.cfg.RecordBudget = 512
	must(t, func() error { _, err := f.db.Exec("CREATE TABLE idle_writes(n INTEGER)"); return err }())
	for _, table := range []string{"guardian_epochs", "guardian_threads", "guardian_turn_models", "guardian_requests", "guardian_cursors", "guardian_positions", "guardian_anomalies", "guardian_anomaly_states"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			_, err := f.db.Exec(fmt.Sprintf("CREATE TRIGGER idle_%s_%s AFTER %s ON %s BEGIN INSERT INTO idle_writes VALUES(1); END", table, event, event, table))
			must(t, err)
		}
	}
	spy := &idleBatchSpy{GuardianStore: f.store}
	f.svc.store = spy
	for range 3 {
		f.now = f.now.Add(time.Minute)
		f.collect()
	}
	if spy.calls != 0 || f.count("idle_writes") != 0 {
		t.Fatalf("idle poll replayed batch or wrote: batches=%d writes=%d", spy.calls, f.count("idle_writes"))
	}
}

func TestLongCollectedPrefixAppendsWithoutCoverageGap(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.cfg.RecordBudget = 4096
	f.write("guardian", largeIdleSource(f))
	f.collect()
	f.svc.cfg.RecordBudget = 512
	for i := range 4 {
		f.now = f.now.Add(time.Minute)
		f.append("guardian", f.request(fmt.Sprintf("tail-%d", i), "t", 100, 20))
		f.collect()
	}
	if len(f.requests()) != 6 || f.codes()[domain.GuardianCoverageGap] != 0 {
		t.Fatalf("normal append mislabeled: requests=%d codes=%v", len(f.requests()), f.codes())
	}
}

type countedSource struct {
	io.ReadSeeker
	bytes int
}

func (s *countedSource) Read(p []byte) (int, error) {
	n, err := s.ReadSeeker.Read(p)
	s.bytes += n
	return n, err
}

func TestIdleCheckpointReadsScaleWithRequestsNotPrefixLines(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.cfg.RecordBudget = 4096
	data := largeIdleSource(f)
	f.write("guardian", data)
	f.collect()
	f.svc.cfg.RecordBudget = 512
	owner := f.owners()[0]
	cursor, err := f.store.GuardianCursor(context.Background(), owner.EpochID, "guardian")
	must(t, err)
	source := &countedSource{ReadSeeker: strings.NewReader(data)}
	changed, err := f.svc.checkpoints(context.Background(), source, cursor)
	must(t, err)
	if changed || source.bytes > 16*1024 {
		t.Fatalf("idle checkpoint scanned prefix: changed=%v bytes=%d", changed, source.bytes)
	}
}

func TestIncrementalRevalidationSurvivesRestartAndDoesNotTreatBudgetAsGap(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.cfg.RecordBudget = 4096
	noise := f.line("unrelated", map[string]string{"padding": strings.Repeat("x", 1800)}, f.base)
	header := f.meta("guardian", "root", true, f.base) + f.request("a", "t", 100, 20)
	old := header + strings.Repeat(noise, 1200) + f.request("b", "t", 100, 20)
	f.write("guardian", old)
	f.collect()
	f.svc.cfg.RecordBudget = 512
	changed := f.request("new", "t", 100, 20)
	changed = strings.TrimSuffix(changed, "\n") + strings.Repeat(" ", len(noise)-len(changed)) + "\n"
	f.write("guardian", header+strings.Repeat(noise, 1000)+changed+strings.Repeat(noise, 199)+f.request("b", "t", 100, 20))
	f.collect()
	if f.codes()[domain.GuardianCoverageGap] != 0 {
		t.Fatal("pending revalidation mislabeled as data gap")
	}
	f.svc = New(f.store, f.svc.cfg)
	for range 3 {
		f.collect()
	}
	if len(f.requests()) != 3 {
		t.Fatalf("incremental verification failed to discover late prefix change: %v", f.requests())
	}
	cursor, err := f.store.GuardianCursor(context.Background(), f.owners()[0].EpochID, "guardian")
	must(t, err)
	if cursor.ReplayUntil != 0 {
		t.Fatal("completed verification did not clear pending interval")
	}
}

func TestModelOnlyVerificationChunkUsesThreadEvidence(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.cfg.RecordBudget = 4096
	data := f.meta("guardian", "root", true, f.base) + f.model("t", "gpt-5.4-mini") + strings.Repeat(f.line("unrelated", map[string]string{"text": "unchanged"}, f.base), 1100) + f.request("r", "t", 100, 20)
	f.write("guardian", data)
	f.collect()
	f.svc.cfg.RecordBudget = 512
	path := filepath.Join(f.root, "guardian.jsonl")
	info, err := os.Stat(path)
	must(t, err)
	must(t, os.Chtimes(path, info.ModTime(), info.ModTime().Add(time.Second)))
	for range 3 {
		f.collect()
	}
	if f.codes()[domain.GuardianCoverageGap] != 0 || len(f.requests()) != 1 {
		t.Fatalf("verification chunk mislabeled: %v", f.codes())
	}
}

func TestCheckpointSamplingMovesPastItsBudget(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	data := f.meta("guardian", "root", true, f.base) + f.request("a", "t", 100, 20) + f.request("b", "t", 100, 20)
	f.write("guardian", data)
	f.collect()
	f.svc.cfg.RecordBudget = 1
	cursor, err := f.store.GuardianCursor(context.Background(), f.owners()[0].EpochID, "guardian")
	must(t, err)
	changed, err := f.svc.checkpoints(context.Background(), strings.NewReader(data), cursor)
	must(t, err)
	if changed {
		t.Fatal("unchanged first sample failed")
	}
	data = strings.Replace(data, `"response_id":"b"`, `"response_id":"c"`, 1)
	changed, err = f.svc.checkpoints(context.Background(), strings.NewReader(data), cursor)
	must(t, err)
	if !changed {
		t.Fatal("bounded sampling never advanced to later request")
	}
}
