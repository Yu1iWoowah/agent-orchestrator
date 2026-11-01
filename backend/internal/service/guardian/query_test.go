package guardian

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestA5A6A7QueryNoInferredLinksAndSafeSummary(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.chat()
	r := f.review("native-review", "execve", true)
	r.ParentTurnID = "shared-root-turn"
	f.recordReview(r)
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.model("turn", "model-a")+f.request("native-response", "turn", 100, 20))
	f.collect()
	before := f.count("model_usage_events")
	service := NewQuery(f.store, func() time.Time { return f.now.Add(time.Second) })
	report, err := service.Query(context.Background(), f.session.ID, QueryOptions{})
	must(t, err)
	if report.Requests != 1 || report.ExactReviewLinks != 0 || report.UnmatchedRequests != 1 || report.Reconciliation.State != "not_comparable" || report.Reconciliation.LedgerSum != nil || report.Reconciliation.Difference != nil {
		t.Fatalf("inferred link: %+v", report)
	}
	if report.Totals.Input == nil || *report.Totals.Input != 100 || *report.Totals.Cached != 20 || *report.Totals.Uncached != 80 || *report.Totals.Output != 20 || *report.Totals.Reasoning != 10 || report.Totals.CacheWrite != nil || report.Totals.Unknown["cache_write"] != 1 {
		t.Fatalf("bad totals: %+v", report.Totals)
	}
	raw, err := json.Marshal(report)
	must(t, err)
	for _, id := range []string{"native-review", "native-response", "shared-root-turn", `"thread_id"`, `"native_epoch"`} {
		if strings.Contains(string(raw), id) {
			t.Fatalf("summary exposed %s", id)
		}
	}
	details, err := service.Query(context.Background(), f.session.ID, QueryOptions{Details: true, Limit: 1})
	must(t, err)
	if len(details.Details) != 1 || details.Details[0].ReviewID != nil || details.Details[0].EvidenceKind != nil || details.NextCursor == "" {
		t.Fatal("request falsely matched or page missing")
	}
	next, err := service.Query(context.Background(), f.session.ID, QueryOptions{Details: true, Limit: 1, Cursor: details.NextCursor})
	must(t, err)
	if len(next.Details) != 1 || next.Details[0].ReviewID == nil || *next.Details[0].ReviewID != "native-review" || next.NextCursor != "" {
		t.Fatalf("bad next page: %+v", next.Details)
	}
	if f.count("model_usage_events") != before {
		t.Fatal("query changed ledger")
	}
	f.enable(false)
	disabled, err := service.Query(context.Background(), f.session.ID, QueryOptions{Details: true})
	must(t, err)
	if disabled.Enabled || disabled.Requests != 0 || len(disabled.Details) != 0 {
		t.Fatal("disabled returned facts")
	}
}
func TestA5TUIUnsupportedAndA10Pagination(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	var source strings.Builder
	source.WriteString(f.meta("guardian", "root", true, f.base))
	for i := 0; i < 503; i++ {
		source.WriteString(f.request(fmt.Sprintf("r-%04d", i), "turn", 100, 10))
	}
	f.write("guardian", source.String())
	f.collect()
	service := NewQuery(f.store, func() time.Time { return f.now.Add(time.Second) })
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 3; page++ {
		report, err := service.Query(context.Background(), f.session.ID, QueryOptions{Details: true, Limit: 250, Cursor: cursor})
		must(t, err)
		if report.Capability.ReviewEvents != "unsupported" || report.Requests != 503 {
			t.Fatalf("bad capability/count: %+v", report.Capability)
		}
		for _, r := range report.Details {
			if r.ResponseID == nil || seen[*r.ResponseID] {
				t.Fatal("duplicate/absent response")
			}
			seen[*r.ResponseID] = true
		}
		cursor = report.NextCursor
	}
	if len(seen) != 503 || cursor != "" {
		t.Fatal("pagination lost requests")
	}
	for _, o := range []QueryOptions{{Details: true, Cursor: "invalid"}, {Limit: 1}, {Details: true, Limit: 501}, {Since: f.now, Until: f.base}} {
		if _, err := service.Query(context.Background(), f.session.ID, o); err == nil {
			t.Fatal("invalid query accepted")
		}
	}
	if _, err := service.Query(context.Background(), "unknown-session", QueryOptions{}); err == nil {
		t.Fatal("unknown session accepted")
	}
}
func TestA6AggregateOverflowIsUnknown(t *testing.T) {
	totals := newTotals()
	maximum := int64(math.MaxInt64)
	one := int64(1)
	zero := int64(0)
	totals.add(domain.GuardianTokens{Input: &maximum, Cached: &zero})
	totals.add(domain.GuardianTokens{Input: &one, Cached: &zero})
	if totals.Input != nil || totals.Uncached != nil || len(totals.Overflow) != 2 || totals.Known["input"] != 2 || totals.Unknown["output"] != 2 {
		t.Fatalf("overflow hidden: %+v", totals)
	}
}

type groupedQueryStore struct {
	QueryStore
	facts     []domain.GuardianFact
	anomalies map[domain.GuardianCode]int64
}

func (s *groupedQueryStore) GuardianAnomalies(context.Context, domain.SessionID) (map[domain.GuardianCode]int64, error) {
	return s.anomalies, nil
}
func (s *groupedQueryStore) PageGuardianFacts(_ context.Context, _ domain.SessionID, _, _, _ time.Time, kind, after string, _ int64) ([]domain.GuardianFact, error) {
	page := make([]domain.GuardianFact, 0, 500)
	for _, fact := range s.facts {
		if fact.Kind == kind && fact.Sequence > after {
			page = append(page, fact)
			if len(page) == 500 {
				break
			}
		}
	}
	return page, nil
}
func TestQueryGroupGapCountsOnceWithoutMutatingStoredAnomalies(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	store := &groupedQueryStore{QueryStore: f.store, anomalies: map[domain.GuardianCode]int64{domain.GuardianCoverageGap: 7}}
	for i := 0; i < 510; i++ {
		input, cached := int64(100), int64(10)
		request := domain.GuardianRequest{EpochID: 1, ThreadID: fmt.Sprintf("guardian-%04d", i), TurnID: "turn", ResponseID: fmt.Sprintf("response-%04d", i), ModelID: fmt.Sprintf("model-%04d", i), CLIVersion: "0.166.0", ProviderAt: f.base, FirstSeenAt: f.now, Tokens: domain.GuardianTokens{Input: &input, Cached: &cached}}
		store.facts = append(store.facts, domain.GuardianFact{Kind: "request", Sequence: fmt.Sprintf("%04d", i), Request: &request})
	}
	review := f.review("unknown-review", "unknown", true)
	store.facts = append(store.facts, domain.GuardianFact{Kind: "review", Sequence: "review", Review: &review})
	service := NewQuery(store, func() time.Time { return f.now.Add(time.Second) })
	for _, details := range []bool{false, true, false} {
		report, err := service.Query(context.Background(), f.session.ID, QueryOptions{Details: details})
		must(t, err)
		if report.Anomalies[domain.GuardianCoverageGap] != 8 || store.anomalies[domain.GuardianCoverageGap] != 7 {
			t.Fatalf("query multiplied/mutated anomaly count: report=%v stored=%v", report.Anomalies, store.anomalies)
		}
		if len(report.ByModel) != 500 || report.Requests != 510 {
			t.Fatal("group cap changed request totals")
		}
		actions := map[string]Distribution{}
		for _, group := range report.ByAction {
			actions[group.Value] = group
		}
		if actions["not_applicable"].Requests != 510 || actions["not_applicable"].Reviews != 0 || actions["unknown"].Requests != 0 || actions["unknown"].Reviews != 1 {
			t.Fatalf("request and review action meanings mixed: %+v", actions)
		}
	}
	store.anomalies[domain.GuardianCoverageGap] = math.MaxInt64
	report, err := service.Query(context.Background(), f.session.ID, QueryOptions{})
	must(t, err)
	if report.Anomalies[domain.GuardianCoverageGap] != math.MaxInt64 {
		t.Fatal("coverage count overflowed")
	}
}

func TestGuardianHistoricalWindowUsesEventTime(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.chat()
	f.recordReview(f.review("late-review", "networkAccess", true))
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("late-request", "turn", 100, 20))
	f.collect()
	svc := NewQuery(f.store, func() time.Time { return f.now })
	for _, tc := range []struct {
		name              string
		at                time.Time
		requests, reviews int64
	}{
		{"request", f.base.Add(2 * time.Minute), 1, 0},
		{"review", f.base.Add(time.Minute), 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, details := range []bool{false, true} {
				report, err := svc.Query(context.Background(), f.session.ID, QueryOptions{Since: tc.at, Until: tc.at.Add(time.Second), Details: details})
				must(t, err)
				if report.Requests != tc.requests || report.Reviews != tc.reviews {
					t.Errorf("historic facts requests/reviews = %d/%d, want %d/%d", report.Requests, report.Reviews, tc.requests, tc.reviews)
				}
				if details && len(report.Details) != 1 {
					t.Errorf("historic details = %d, want 1", len(report.Details))
				}
				exclusive, err := svc.Query(context.Background(), f.session.ID, QueryOptions{Since: tc.at.Add(-time.Second), Until: tc.at})
				must(t, err)
				if exclusive.Requests != 0 || exclusive.Reviews != 0 {
					t.Fatal("until must be exclusive")
				}
			}
		})
	}
}

func TestGuardianHistoricalCursorExcludesLaterIngestion(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.chat()
	f.recordReview(f.review("review-a", "networkAccess", true))
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("request-a", "turn", 100, 20))
	f.collect()
	service := NewQuery(f.store, func() time.Time { return f.now })
	options := QueryOptions{Since: f.base, Until: f.base.Add(3 * time.Minute), Details: true, Limit: 1}
	first, err := service.Query(context.Background(), f.session.ID, options)
	must(t, err)
	if len(first.Details) != 1 || first.NextCursor == "" || first.Requests != 1 || first.Reviews != 1 {
		t.Fatal("initial late-ingestion page missing")
	}
	// Ingest event times inside the same historical window after page one.
	f.now = f.now.Add(time.Minute)
	f.recordReview(f.review("review-b", "execve", true))
	f.append("guardian", f.request("request-b", "turn", 200, 10))
	f.collect()
	next, err := service.Query(context.Background(), f.session.ID, QueryOptions{Details: true, Limit: 1, Cursor: first.NextCursor})
	must(t, err)
	if next.Requests != 1 || next.Reviews != 1 || len(next.Details) != 1 || next.Details[0].ReviewID == nil || *next.Details[0].ReviewID != "review-a" || next.NextCursor != "" {
		t.Fatalf("later ingestion changed cursor membership: %+v", next)
	}
	fresh, err := service.Query(context.Background(), f.session.ID, options)
	must(t, err)
	if fresh.Requests != 2 || fresh.Reviews != 2 {
		t.Fatalf("fresh snapshot misses facts: %d/%d", fresh.Requests, fresh.Reviews)
	}
}
