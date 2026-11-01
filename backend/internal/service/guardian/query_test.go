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
