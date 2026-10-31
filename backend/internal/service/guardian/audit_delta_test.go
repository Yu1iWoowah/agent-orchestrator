package guardian

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestDeltaPrefixRewriteKeepsLastCheckpoint(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	head := f.meta("guardian", "root", true, f.base)
	tail := f.request("end", "t", 200, 20)
	f.write("guardian", head+f.request("old", "t", 100, 20)+tail)
	f.collect()
	path := filepath.Join(f.root, "guardian.jsonl")
	before, err := os.Stat(path)
	must(t, err)
	f.write("guardian", head+f.request("new", "t", 100, 20)+tail)
	after, err := os.Stat(path)
	must(t, err)
	if !os.SameFile(before, after) || before.Size() != after.Size() {
		t.Fatal("invalid fixture")
	}
	f.collect()
	t.Logf("size=%d requests=%+v codes=%v", after.Size(), f.requests(), f.codes())
	if len(f.requests()) != 3 || f.codes()[domain.GuardianSourceTruncated] == 0 {
		t.Fatal("unchanged last checkpoint hid rewritten prefix response")
	}
}
func TestDeltaKnownOwnerBrokenParentRetainsCoverage(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("old", "t", 100, 20))
	f.collect()
	if len(f.requests()) != 1 || len(f.codes()) != 0 {
		t.Fatal("invalid known-owner fixture")
	}
	f.write("guardian", f.meta("guardian", "missing", true, f.base)+f.request("new", "t", 100, 20))
	f.collect()
	codes := f.codes()
	t.Logf("requests=%+v codes=%v", f.requests(), codes)
	if codes[domain.GuardianOwnerUnresolved]+codes[domain.GuardianOwnerConflict]+codes[domain.GuardianSourceConflict]+codes[domain.GuardianCoverageGap] == 0 {
		t.Fatal("known owner lost all coverage for changed broken lineage")
	}
}
func TestDeltaExpiredCheckpointDoesNotReportTruncation(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("old", "t", 100, 20))
	f.collect()
	f.now = f.base.Add(domain.GuardianRetention - 28*time.Minute)
	f.collect()
	if f.count("guardian_requests") != 1 || len(f.codes()) != 0 {
		t.Fatal("invalid pre-expiry fixture")
	}
	f.now = f.now.Add(31 * time.Minute)
	f.collect()
	t.Logf("retained_rows=%d codes=%v", f.count("guardian_requests"), f.codes())
	if f.codes()[domain.GuardianSourceTruncated] != 0 {
		t.Fatal("retention expiry misclassified as physical source truncation")
	}
}

func TestPrefixReplayFindsUsageIntroducedBetweenKnownRequests(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	header := f.meta("guardian", "root", true, f.base)
	introduced := f.request("new", "t", 100, 20)
	unknown := `{"type":"unrelated","payload":{}}`
	unknown += strings.Repeat(" ", len(introduced)-len(unknown)-1) + "\n"
	f.write("guardian", header+unknown+f.request("end", "t", 200, 20))
	f.collect()
	f.write("guardian", header+introduced+f.request("end", "t", 200, 20))
	f.collect()
	if len(f.requests()) != 2 || f.codes()[domain.GuardianCoverageGap] == 0 {
		t.Fatalf("unproven prefix change silently lost usage: %v %v", f.requests(), f.codes())
	}
}

func TestPrefixVerificationBudgetDeclaresGapWithoutFalseTruncation(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("a", "t", 100, 20)+f.request("b", "t", 100, 20))
	f.collect()
	f.svc.cfg.RecordBudget = 1
	f.collect()
	if f.codes()[domain.GuardianCoverageGap] == 0 || f.codes()[domain.GuardianSourceTruncated] != 0 {
		t.Fatalf("unverified prefix mislabeled: %v", f.codes())
	}
	seq, err := f.store.LatestSeq(context.Background())
	must(t, err)
	f.collect()
	latest, err := f.store.LatestSeq(context.Background())
	must(t, err)
	if latest != seq {
		t.Fatal("unchanged prefix budget emitted repeated CDC")
	}
}
