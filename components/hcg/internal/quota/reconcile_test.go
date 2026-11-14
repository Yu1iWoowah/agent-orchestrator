package quota

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/usage"
)

func quotaSample(root string, minute int, used float64) Sample {
	raw := []byte(fmt.Sprintf(`{"accounts":[{"id":"a","provider":"claude","limits":{"fetchedAt":"2026-11-09T00:%02d:00Z","windows":[{"id":"w","label":"arbitrary window","usedPercent":%v,"resetsAt":"2026-11-09T05:00:00Z"}]}}]}`, minute, used))
	return Project(raw, nil, root, time.Date(2026, 11, 9, 0, minute+1, 0, 0, time.UTC))
}
func TestReconcileQuotaAndUnassignedLedger(t *testing.T) {
	query, err := usage.NewCohort([]string{"s"}, "2026-11-09T00:00:00Z", "2026-11-09T01:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 9, 0, 5, 0, 0, time.UTC)
	n := int64(100)
	zero := int64(0)
	id := "current-account-not-historical-proof"
	base := usage.Snapshot{Source: "daemon_ledger", DataRoot: "/fixture", ScanStatus: "complete", Page: usage.Page{SessionID: "s", AccountID: &id}}
	for _, tc := range []struct {
		name, status, reason string
		mutate               func([]Sample, *usage.Snapshot)
	}{
		{"rise_no_events", "unaccounted_usage_candidate", "no_recorded_events", func(_ []Sample, _ *usage.Snapshot) {}},
		{"together", "observed_together", "temporal_overlap_only_account_unassigned", func(_ []Sample, s *usage.Snapshot) {
			s.Events = []usage.Event{{ID: 7, BindingID: 2, UsageSourceID: 3, CreatedAt: &at, InputTokens: &n, CachedInputTokens: &zero, UncachedInputTokens: &n, OutputTokens: &zero}}
		}},
		{"repeated", "not_comparable", "repeated_fetched_at", func(rows []Sample, _ *usage.Snapshot) { rows[1].Accounts[0].FetchedAt = rows[0].Accounts[0].FetchedAt }},
		{"reset", "not_comparable", "reset_or_rolling", func(rows []Sample, _ *usage.Snapshot) {
			next := rows[1].Accounts[0].Windows[0].ResetsAt.Add(time.Hour)
			rows[1].Accounts[0].Windows[0].ResetsAt = &next
		}},
		{"rolling", "not_comparable", "reset_or_rolling", func(rows []Sample, _ *usage.Snapshot) { v := 5.0; rows[1].Accounts[0].Windows[0].UsedPercent = &v }},
		{"missing_reset", "not_comparable", "reset_missing", func(rows []Sample, _ *usage.Snapshot) { rows[1].Accounts[0].Windows[0].ResetsAt = nil }},
		{"stale", "not_comparable", "stale_or_future", func(rows []Sample, _ *usage.Snapshot) { rows[1].SampledAt = rows[1].SampledAt.Add(10 * time.Minute) }},
		{"gap", "not_comparable", "sampling_gap", func(rows []Sample, _ *usage.Snapshot) {
			rows[1].SampledAt = rows[1].SampledAt.Add(30 * time.Minute)
			f := rows[1].SampledAt
			rows[1].Accounts[0].FetchedAt = &f
		}},
		{"unknown_time", "not_comparable", "undated_events", func(_ []Sample, s *usage.Snapshot) { s.Events = []usage.Event{{ID: 9, InputTokens: &n}} }},
		{"partial", "not_comparable", "ledger_quality", func(_ []Sample, s *usage.Snapshot) { s.ScanStatus = "partial" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []Sample{quotaSample("/fixture", 0, 10), quotaSample("/fixture", 10, 15)}
			snap := base
			tc.mutate(rows, &snap)
			got, err := Reconcile(query, rows, []usage.Snapshot{snap}, "/fixture", 2*time.Minute, 20*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			last := got.Segments[len(got.Segments)-1]
			if last.Status != tc.status || last.Reason != tc.reason {
				t.Fatalf("segment=%+v", last)
			}
			if tc.name == "rise_no_events" && (last.DeltaPercentagePoints == nil || *last.DeltaPercentagePoints != 5) {
				t.Fatalf("delta=%+v", last)
			}
			if tc.name == "rolling" && (last.DeltaPercentagePoints == nil || *last.DeltaPercentagePoints != -5) {
				t.Fatal("negative net change was hidden")
			}
			if tc.name == "together" && (got.Unassigned.Recorded.EventCount != 1 || *got.Unassigned.Recorded.InputTokens != 100 || len(last.EventRefs) != 1 || last.EventRefs[0].EventID != 7 || last.Attribution != "unassigned") {
				t.Fatalf("lost unassigned evidence: %+v", got)
			}
		})
	}
	rows := []Sample{quotaSample("/fixture", 0, 10), quotaSample("/fixture", 10, 15)}
	rows[1].Accounts[0].ID = "switched"
	got, err := Reconcile(query, rows, []usage.Snapshot{base}, "/fixture", 2*time.Minute, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range got.Segments {
		if segment.Status != "not_comparable" {
			t.Fatalf("account switch guessed: %+v", segment)
		}
	}
	// A failed intermediate sample must break comparison, not bridge its gap.
	bad := Project(nil, fmt.Errorf("raw secret"), "/fixture", rows[0].SampledAt.Add(5*time.Minute))
	rows[1].Accounts[0].ID = "a"
	got, err = Reconcile(query, []Sample{rows[0], bad, rows[1]}, []usage.Snapshot{base}, "/fixture", 2*time.Minute, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range got.Segments {
		if segment.Status == "unaccounted_usage_candidate" {
			t.Fatal("bridged failed sample")
		}
	}
}

func TestReconcileRetainsLedgerFailureStatus(t *testing.T) {
	query, err := usage.NewCohort([]string{"s"}, "2026-11-09T00:00:00Z", "2026-11-09T01:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 9, 0, 5, 0, 0, time.UTC)
	huge, one, zero := int64(math.MaxInt64), int64(1), int64(0)
	scans := []usage.Snapshot{{Source: "daemon_ledger", DataRoot: "/fixture", ScanStatus: "complete", Page: usage.Page{SessionID: "s", Events: []usage.Event{{ID: 1, CreatedAt: &at, InputTokens: &huge, CachedInputTokens: &zero, UncachedInputTokens: &huge, OutputTokens: &zero}, {ID: 2, CreatedAt: &at, InputTokens: &one, CachedInputTokens: &zero, UncachedInputTokens: &one, OutputTokens: &zero}}}}}
	got, err := Reconcile(query, []Sample{quotaSample("/fixture", 0, 10), quotaSample("/fixture", 10, 15)}, scans, "/fixture", 2*time.Minute, 20*time.Minute)
	if err == nil || got.Status != "partial" || got.Unassigned.Status != "partial" || got.Unassigned.Recorded.EventCount != 1 {
		t.Fatalf("status=%s ledger=%+v err=%v", got.Status, got.Unassigned, err)
	}
	scans[0].Source = "external_analysis"
	got, err = Reconcile(query, nil, scans, "/fixture", 2*time.Minute, 20*time.Minute)
	if err == nil || got.Status != "unavailable" {
		t.Fatalf("noncanonical status=%s err=%v", got.Status, err)
	}
}
