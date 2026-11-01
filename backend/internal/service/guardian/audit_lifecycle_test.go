package guardian

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestD2FreshAnomalySurvivesOldEpochCleanup(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("old", "t", 100, 20))
	f.collect()
	old := f.owners()[0].EpochID
	must(t, os.Remove(filepath.Join(f.root, "guardian.jsonl")))
	f.now = f.base.Add(29 * 24 * time.Hour)
	f.write("bad-new", strings.ReplaceAll(f.meta("bad-new", "root", true, f.now.Add(-time.Minute)), "0.166.0", "9.999.0"))
	f.collect()
	if f.codes()[domain.GuardianUnsupportedVersion] != 1 {
		t.Fatal("invalid fresh anomaly fixture")
	}
	f.now = f.base.Add(31 * 24 * time.Hour)
	f.collect()
	t.Logf("old_epoch=%d new_epoch=%d states=%d codes=%v", old, f.owners()[0].EpochID, f.count("guardian_anomaly_states"), f.codes())
	if f.codes()[domain.GuardianUnsupportedVersion] != 1 {
		t.Fatal("fresh anomaly dedupe was cascaded with an expired epoch")
	}
}
func TestD2KnownLineageEndingAtUnclaimedRootReportsConflict(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("old", "t", 100, 20))
	f.collect()
	if len(f.requests()) != 1 || len(f.codes()) != 0 {
		t.Fatal("invalid proven-owner fixture")
	}
	f.write("unclaimed", f.meta("unclaimed", "", false, f.base))
	f.write("guardian", f.meta("guardian", "unclaimed", true, f.base)+f.request("new", "t", 100, 20))
	f.collect()
	codes := f.codes()
	t.Logf("requests=%v codes=%v", f.requests(), codes)
	if codes[domain.GuardianOwnerUnresolved]+codes[domain.GuardianOwnerConflict]+codes[domain.GuardianSourceConflict]+codes[domain.GuardianCoverageGap] == 0 {
		t.Fatal("known lineage changed to an unclaimed root without coverage")
	}
}
