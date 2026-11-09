package usage

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func token(v int64) *int64 { return &v }
func eventAt(at time.Time, id, source, input, cached, output int64) Event {
	return Event{MeasurementKind: "native_reported", ID: id, BindingID: 1, UsageSourceID: source, CreatedAt: &at, InputTokens: token(input), CachedInputTokens: token(cached), UncachedInputTokens: token(input - cached), OutputTokens: token(output)}
}

func TestBaselineCanonicalCohort(t *testing.T) {
	from := "2026-11-08T00:00:00Z"
	to := "2026-11-09T00:00:00Z"
	query, err := NewCohort([]string{"s1", "s2"}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := time.Parse(time.RFC3339, from)
	end, _ := time.Parse(time.RFC3339, to)
	first := eventAt(start, 1, 1, 100, 70, 5)
	first.ProviderUsage = map[string]any{"input_tokens": 99999}
	s1 := Snapshot{Source: "daemon_ledger", ScanStatus: "complete", Page: Page{SessionID: "s1", Consistency: "insertion_bound_only; existing rows and metadata may change", Bindings: []Binding{{ID: 1, SessionID: "s1", Harness: "claude-code", NativeRootID: "root", State: "active", Sources: []Source{{ID: 1, Kind: "claude_main", NativeSessionID: "root", State: "active"}, {ID: 2, Kind: "claude_subagent", NativeSessionID: "child", SubagentID: "child", State: "error", AnomalyCount: 1}, {ID: 99, Kind: "future", State: "pending"}}}}, Events: []Event{first, eventAt(start.Add(time.Hour), 2, 2, 200, 0, 10), eventAt(end, 3, 1, 999, 0, 99), {ID: 4, MeasurementKind: "unknown", BindingID: 1, UsageSourceID: 1, CachedInputTokens: token(20), OutputTokens: token(2)}, eventAt(start.Add(-time.Second), 5, 1, 500, 0, 5)}}}
	s2 := s1
	s2.SessionID = "s2"
	s2.Bindings = nil
	s2.Events = []Event{eventAt(start, 6, 3, 0, 0, 0)}
	got, err := Baseline(query, []Snapshot{s1, s2})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "daemon_ledger" || got.Status != "complete" || got.From != from || got.To != to || len(got.SessionIDs) != 2 || got.Sessions[0].Consistency != s1.Consistency || got.SessionIDs[0] != "s1" || got.CaptureRate != nil {
		t.Fatalf("manifest=%+v", got)
	}
	if got.Recorded.EventCount != 3 || got.Recorded.InputTokens == nil || *got.Recorded.InputTokens != 300 || *got.Recorded.CachedInputTokens != 70 || *got.Recorded.OutputTokens != 15 {
		t.Fatalf("recorded=%+v", got.Recorded)
	}
	if got.Undated.EventCount != 1 || got.Undated.InputTokens != nil || got.Undated.IncompleteEvents != 1 || *got.Undated.CachedInputTokens != 20 {
		t.Fatalf("undated=%+v", got.Undated)
	}
	sources := got.Sessions[0].Sources
	if len(sources) != 3 || sources[0].Scope != "root" || sources[1].Scope != "child" || *sources[1].Recorded.InputTokens != 200 || sources[1].Source.AnomalyCount != 1 || len(sources[0].MeasurementKinds) != 2 || sources[0].MeasurementKinds[0] != "native_reported" || sources[0].MeasurementKinds[1] != "unknown" || sources[2].Scope != "unknown" || sources[2].Recorded.InputTokens != nil {
		t.Fatalf("source rows=%+v", sources)
	}
	again, err := Baseline(query, []Snapshot{s1, s2})
	if err != nil || *again.Recorded.InputTokens != *got.Recorded.InputTokens {
		t.Fatalf("same cohort changed: %+v %v", again, err)
	}
	// Unknown source metadata does not downgrade an existing token vector.
	if got.Sessions[1].Recorded.IncompleteEvents != 0 || got.Sessions[1].Sources[0].Scope != "unknown" {
		t.Fatalf("metadata=%+v", got.Sessions[1])
	}
}

func TestCohortValidation(t *testing.T) {
	for _, tc := range []struct {
		ids      []string
		from, to string
	}{
		{nil, "2026-11-08T00:00:00Z", "2026-11-09T00:00:00Z"},
		{[]string{"s", "s"}, "2026-11-08T00:00:00Z", "2026-11-09T00:00:00Z"},
		{[]string{""}, "2026-11-08T00:00:00Z", "2026-11-09T00:00:00Z"},
		{[]string{" s"}, "2026-11-08T00:00:00Z", "2026-11-09T00:00:00Z"},
		{[]string{"s"}, "2026-11-08", "2026-11-09T00:00:00Z"},
		{[]string{"s"}, "2026-11-08T00:00:00+08:00", "2026-11-09T00:00:00Z"},
		{[]string{"s"}, "2026-11-09T00:00:00Z", "2026-11-09T00:00:00Z"},
	} {
		if _, err := NewCohort(tc.ids, tc.from, tc.to); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
}

func TestBaselinePartialAndExternal(t *testing.T) {
	query, err := NewCohort([]string{"s"}, "2026-11-08T00:00:00Z", "2026-11-09T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	snapshot := Snapshot{Source: "daemon_ledger", ScanStatus: "partial", Page: Page{SessionID: "s", Events: []Event{eventAt(at, 1, 1, 100, 70, 5)}}}
	got, err := Baseline(query, []Snapshot{snapshot})
	if err != nil || got.Status != "partial" || *got.Recorded.InputTokens != 100 || got.CaptureRate != nil {
		t.Fatalf("partial=%+v err=%v", got, err)
	}
	snapshot.Source = "external_analysis"
	got, err = Baseline(query, []Snapshot{snapshot})
	if err == nil || got.Recorded.EventCount != 0 || got.Recorded.InputTokens != nil {
		t.Fatalf("external merged: %+v %v", got, err)
	}
}

func TestBaselineOverflowPreservesAcceptedPrefix(t *testing.T) {
	for _, counter := range []string{"input", "output"} {
		for _, undated := range []bool{false, true} {
			for _, unknownCohort := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/undated=%t/unknownCohort=%t", counter, undated, unknownCohort), func(t *testing.T) {
					ids := []string{"s"}
					if unknownCohort {
						ids = []string{"prior", "s"}
					}
					query, err := NewCohort(ids, "2026-11-08T00:00:00Z", "2026-11-09T00:00:00Z")
					if err != nil {
						t.Fatal(err)
					}
					at := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
					first, second := eventAt(at, 1, 1, math.MaxInt64, 0, 5), eventAt(at, 2, 1, 1, 0, 7)
					if counter == "output" {
						first, second = eventAt(at, 1, 1, 10, 0, math.MaxInt64), eventAt(at, 2, 1, 20, 0, 1)
					}
					if undated {
						first.CreatedAt, second.CreatedAt = nil, nil
					}
					snapshots := []Snapshot{{Source: "daemon_ledger", ScanStatus: "complete", Page: Page{SessionID: "s", Events: []Event{first, second}}}}
					if unknownCohort {
						prior := Event{ID: 0, BindingID: 1, UsageSourceID: 1, CreatedAt: first.CreatedAt}
						snapshots = append([]Snapshot{{Source: "daemon_ledger", ScanStatus: "complete", Page: Page{SessionID: "prior", Events: []Event{prior}}}}, snapshots...)
					}
					got, err := Baseline(query, snapshots)
					if err == nil || got.Status != "partial" || got.Reason != "token sum exceeds int64" {
						t.Fatalf("overflow=%+v err=%v", got, err)
					}
					row := got.Sessions[len(got.Sessions)-1]
					cohort, session, source := got.Recorded, row.Recorded, row.Sources[0].Recorded
					if undated {
						cohort, session, source = got.Undated, row.Undated, row.Sources[0].Undated
					}
					want := TokenTotals{EventCount: 1, InputTokens: first.InputTokens, CachedInputTokens: first.CachedInputTokens, UncachedInputTokens: first.UncachedInputTokens, OutputTokens: first.OutputTokens}
					for name, total := range map[string]TokenTotals{"session": session, "source": source} {
						if !reflect.DeepEqual(total, want) {
							t.Errorf("%s totals=%+v want=%+v", name, total, want)
						}
					}
					if unknownCohort {
						want = TokenTotals{EventCount: 2, IncompleteEvents: 1}
					}
					if !reflect.DeepEqual(cohort, want) {
						t.Errorf("cohort totals=%+v want=%+v", cohort, want)
					}
				})
			}
		}
	}
}

func TestAuxiliaryReceiptExcludedFromNativeTotalsAndCurrent(t *testing.T) {
	q, err := NewCohort([]string{"s"}, "2026-11-09T00:00:00Z", "2026-11-10T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	at := q.start
	native := eventAt(at, 1, 1, 10, 0, 2)
	auxiliary := eventAt(at, 2, 0, 100, 0, 20)
	auxiliary.BindingID = 0
	auxiliary.MeasurementKind = "provider_reported"
	auxiliary.Execution = &ExecutionSource{ID: 1, ProjectID: "p", SessionID: "s", BillingProvider: "openai", ProfileRevision: "v1"}
	snapshot := Snapshot{Source: "daemon_ledger", ScanStatus: "complete", Page: Page{SessionID: "s", ProjectID: "p", CurrentNativeID: "root", Bindings: []Binding{{ID: 1, SessionID: "s", Harness: "codex", NativeRootID: "root", State: "active", Sources: []Source{{ID: 1, Kind: "codex_rollout", NativeSessionID: "root", State: "active"}}}}, Events: []Event{native}}}
	before := Current(snapshot)
	snapshot.Events = append(snapshot.Events, auxiliary)
	if after := Current(snapshot); !reflect.DeepEqual(before, after) {
		t.Fatalf("auxiliary changed current: %+v %+v", before, after)
	}
	got, err := Baseline(q, []Snapshot{snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if got.Recorded.EventCount != 1 || *got.Recorded.InputTokens != 10 || got.AuxiliaryRecorded.EventCount != 1 || *got.AuxiliaryRecorded.InputTokens != 100 || len(got.Sessions[0].Sources) != 2 || got.Sessions[0].Sources[1].Scope != "auxiliary" || got.Sessions[0].Sources[1].Execution.ProfileRevision != "v1" {
		t.Fatalf("baseline=%+v", got)
	}
}
