package guardian

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// QueryStore exposes stored facts only. Reads do not invoke discovery or models.
type QueryStore interface {
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	PageGuardianFacts(context.Context, domain.SessionID, time.Time, time.Time, time.Time, string, string, int64) ([]domain.GuardianFact, error)
	GuardianSnapshot(context.Context) (int64, error)
	GuardianAnomalies(context.Context, domain.SessionID) (map[domain.GuardianCode]int64, error)
	PurgeGuardian(context.Context, domain.SessionID, time.Time) error
}

// QueryOptions is shared by the daemon query and its boundary validation.
type QueryOptions struct {
	Since   time.Time
	Until   time.Time
	Details bool
	Limit   int
	Cursor  string
}

// Report is a whitelist projection, independent of Chat detail and accounting.
type Report struct {
	SchemaVersion     int                           `json:"schema_version"`
	Enabled           bool                          `json:"enabled"`
	Window            Window                        `json:"window"`
	Capability        Capability                    `json:"capability"`
	Coverage          string                        `json:"coverage"`
	Anomalies         map[domain.GuardianCode]int64 `json:"anomalies"`
	Requests          int64                         `json:"requests"`
	Reviews           int64                         `json:"reviews"`
	ExactReviewLinks  int64                         `json:"exact_review_links"`
	UnmatchedRequests int64                         `json:"unmatched_requests"`
	ExcludedConflicts int64                         `json:"excluded_conflicts"`
	IncompleteReviews int64                         `json:"incomplete_reviews"`
	Totals            Totals                        `json:"totals"`
	Buckets           []Distribution                `json:"cache_buckets"`
	ByModel           []Distribution                `json:"by_model"`
	ByVersion         []Distribution                `json:"by_version"`
	ByAction          []Distribution                `json:"by_action"`
	ByThread          []Distribution                `json:"by_guardian_thread,omitempty"`
	Reconciliation    Reconciliation                `json:"ledger_reconciliation"`
	ReuseState        string                        `json:"reuse_state"`
	TranscriptMode    *string                       `json:"transcript_mode"`
	ResetReason       *string                       `json:"reset_reason"`
	UnavailableReason string                        `json:"unavailable_reason"`
	Details           []Detail                      `json:"details,omitempty"`
	NextCursor        string                        `json:"next_cursor,omitempty"`
	Limit             int                           `json:"limit"`
}

// Window is the UTC half-open report interval.
type Window struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// Capability separates review notifications from unavailable direct links.
type Capability struct {
	Provider           string `json:"provider"`
	ReviewEvents       string `json:"review_events"`
	ReviewAvailability string `json:"review_availability"`
	DirectReviewLinks  string `json:"direct_review_links"`
	ContextDecisions   string `json:"context_decisions"`
}

// Totals sums known values only. Unknown counts and overflow are explicit;
// a missing counter is never treated as an observed zero.
type Totals struct {
	Input      *int64           `json:"input_tokens"`
	Cached     *int64           `json:"cached_input_tokens"`
	Uncached   *int64           `json:"uncached_input_tokens"`
	CacheWrite *int64           `json:"cache_write_input_tokens"`
	Output     *int64           `json:"output_tokens"`
	Reasoning  *int64           `json:"reasoning_output_tokens"`
	Total      *int64           `json:"total_tokens"`
	Known      map[string]int64 `json:"known_fields"`
	Unknown    map[string]int64 `json:"unknown_fields"`
	Overflow   []string         `json:"overflow_fields"`
}

// Distribution is one bounded group of independently summed facts.
type Distribution struct {
	Value             string `json:"value"`
	Requests          int64  `json:"requests"`
	Reviews           int64  `json:"reviews"`
	Unmatched         int64  `json:"unmatched_requests"`
	ExcludedConflicts int64  `json:"excluded_conflicts"`
	Totals            Totals `json:"totals"`
}

// Reconciliation compares sources only when scope and baseline are proven.
type Reconciliation struct {
	State      string `json:"state"`
	RequestSum *int64 `json:"request_input_sum"`
	LedgerSum  *int64 `json:"ledger_input_sum"`
	Difference *int64 `json:"difference"`
	Reason     string `json:"unavailable_reason"`
}

// Detail contains native IDs only on explicit request. Link fields stay null
// until an upstream direct-evidence contract is separately implemented.
type Detail struct {
	Kind              string            `json:"kind"`
	NativeEpoch       int64             `json:"native_epoch"`
	ThreadID          string            `json:"thread_id"`
	TurnID            string            `json:"turn_id"`
	ReviewID          *string           `json:"review_id"`
	ResponseID        *string           `json:"response_id"`
	RootTurnID        *string           `json:"root_turn_id"`
	TargetItemID      *string           `json:"target_item_id"`
	Action            *string           `json:"action"`
	Status            *string           `json:"status"`
	DecisionSource    *string           `json:"decision_source"`
	ModelID           *string           `json:"model_id"`
	ModelSource       *string           `json:"model_source"`
	CLIVersion        *string           `json:"cli_version"`
	ProviderAt        *time.Time        `json:"provider_at"`
	StartedAt         *time.Time        `json:"started_at"`
	CompletedAt       *time.Time        `json:"completed_at"`
	FirstSeenAt       time.Time         `json:"first_seen_at"`
	Tokens            *Counters         `json:"tokens"`
	Availability      map[string]string `json:"availability"`
	Reconciliation    string            `json:"reconciliation"`
	EvidenceKind      *string           `json:"evidence_kind"`
	UnavailableReason string            `json:"unavailable_reason"`
}

// Counters keeps missing provider values distinct from observed zero.
type Counters struct {
	Input      *int64 `json:"input_tokens"`
	Cached     *int64 `json:"cached_input_tokens"`
	CacheWrite *int64 `json:"cache_write_input_tokens"`
	Output     *int64 `json:"output_tokens"`
	Reasoning  *int64 `json:"reasoning_output_tokens"`
	Total      *int64 `json:"total_tokens"`
}

// QueryService projects retained facts without discovery or inference.
type QueryService struct {
	store QueryStore
	now   func() time.Time
}

// NewQuery constructs a read service with an injectable clock.
func NewQuery(store QueryStore, now func() time.Time) *QueryService {
	if now == nil {
		now = time.Now
	}
	return &QueryService{store, now}
}

type pageToken struct {
	Session  domain.SessionID `json:"s"`
	Window   Window           `json:"w"`
	Kind     string           `json:"k"`
	Sequence string           `json:"n"`
	Snapshot *int64           `json:"i"`
}

func invalidQuery() error {
	return apierr.Invalid("INVALID_GUARDIAN_QUERY", "Use a valid UTC window and a details page limit between 1 and 500.", nil)
}

// ValidateQuery is applied at both public boundaries; cursor scope is checked
// separately against the resolved session and window.
func ValidateQuery(o QueryOptions) error {
	if o.Limit < 0 || o.Limit > 500 || (!o.Details && (o.Cursor != "" || o.Limit != 0)) || len(o.Cursor) > 2048 {
		return invalidQuery()
	}
	for _, at := range []time.Time{o.Since, o.Until} {
		if !at.IsZero() {
			_, offset := at.Zone()
			if offset != 0 {
				return invalidQuery()
			}
		}
	}
	if !o.Since.IsZero() && !o.Until.IsZero() && !o.Since.Before(o.Until) {
		return invalidQuery()
	}
	return nil
}

// Query derives a safe report and an optional bounded native metadata page.
func (s *QueryService) Query(ctx context.Context, id domain.SessionID, o QueryOptions) (Report, error) {
	if err := ValidateQuery(o); err != nil {
		return Report{}, err
	}
	now := s.now().UTC()
	token := pageToken{Session: id}
	if o.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(o.Cursor)
		if err != nil {
			return Report{}, invalidQuery()
		}
		if json.Unmarshal(raw, &token) != nil || token.Session != id || (token.Kind != "request" && token.Kind != "review") || token.Snapshot == nil || *token.Snapshot < 0 || token.Sequence == "" || len(token.Sequence) > 800 || token.Window.Since.IsZero() || token.Window.Until.IsZero() || !token.Window.Since.Before(token.Window.Until) || token.Window.Until.After(now) {
			return Report{}, invalidQuery()
		}
		if !o.Since.IsZero() && !o.Since.Equal(token.Window.Since) || !o.Until.IsZero() && !o.Until.Equal(token.Window.Until) {
			return Report{}, invalidQuery()
		}
		o.Since = token.Window.Since
		o.Until = token.Window.Until
	}
	if o.Until.IsZero() {
		o.Until = now
	}
	if o.Since.IsZero() {
		o.Since = now.Add(-domain.GuardianRetention)
	}
	if !o.Since.Before(o.Until) || o.Until.After(now) {
		return Report{}, invalidQuery()
	}
	record, found, err := s.store.GetSession(ctx, id)
	if err != nil {
		return Report{}, err
	}
	if !found {
		return Report{}, apierr.NotFound("SESSION_NOT_FOUND", "Session not found.")
	}
	report := Report{SchemaVersion: 1, Window: Window{o.Since.UTC(), o.Until.UTC()}, Coverage: "disabled", Capability: Capability{Provider: string(record.Harness), ReviewEvents: "unsupported", ReviewAvailability: "unsupported", DirectReviewLinks: "unsupported", ContextDecisions: "unsupported"}, Anomalies: map[domain.GuardianCode]int64{}, Totals: newTotals(), Buckets: []Distribution{}, ByModel: []Distribution{}, ByVersion: []Distribution{}, ByAction: []Distribution{}, ReuseState: "unknown", UnavailableReason: "upstream_not_exposed", Reconciliation: Reconciliation{State: "not_comparable", Reason: "source_missing"}}
	if record.ProjectID != "" {
		p, ok, e := s.store.GetProject(ctx, string(record.ProjectID))
		if e != nil {
			return Report{}, e
		}
		report.Enabled = ok && p.Config.GuardianObservability.Enabled
	}
	if !report.Enabled {
		return report, nil
	}
	report.Coverage = "partial"
	if record.Harness != domain.HarnessCodex {
		report.Coverage = "unsupported"
		return report, nil
	}
	if record.Mode == domain.SessionModeChat {
		report.Capability.ReviewEvents = "supported"
		report.Capability.ReviewAvailability = "missing_event"
	}
	snapshot, err := s.store.GuardianSnapshot(ctx)
	if err != nil {
		return Report{}, err
	}
	if token.Snapshot != nil {
		if *token.Snapshot > snapshot {
			return Report{}, invalidQuery()
		}
		snapshot = *token.Snapshot
	}
	storedAnomalies, err := s.store.GuardianAnomalies(ctx, id)
	if err != nil {
		return Report{}, err
	}
	for code, count := range storedAnomalies {
		report.Anomalies[code] = count
	}
	if o.Details {
		report.Limit = o.Limit
		if report.Limit == 0 {
			report.Limit = 100
		}
	}
	groups := map[string]map[string]*Distribution{"bucket": {}, "model": {}, "version": {}, "action": {}, "thread": {}}
	groupGap := false
	more := false
	last := token
	for _, kind := range []string{"request", "review"} {
		after := ""
		for {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			facts, e := s.store.PageGuardianFacts(ctx, id, o.Since, o.Until, now.Add(-domain.GuardianRetention), kind, after, snapshot)
			if e != nil {
				return Report{}, e
			}
			for _, fact := range facts {
				after = fact.Sequence
				if r := fact.Request; r != nil {
					report.Requests++
					values := map[string]string{"bucket": r.Tokens.CacheBucket(), "model": r.ModelID, "version": r.CLIVersion, "action": "not_applicable"}
					if o.Details {
						values["thread"] = r.ThreadID
					}
					if r.Conflict {
						report.ExcludedConflicts++
					} else {
						report.UnmatchedRequests++
						report.Totals.add(r.Tokens)
					}
					for group, value := range values {
						if value == "" {
							value = "unknown"
						}
						g := groups[group][value]
						if g == nil {
							if len(groups[group]) >= 500 {
								groupGap = true
								continue
							}
							g = &Distribution{Value: value, Totals: newTotals()}
							groups[group][value] = g
						}
						g.Requests++
						if r.Conflict {
							g.ExcludedConflicts++
						} else {
							g.Unmatched++
							g.Totals.add(r.Tokens)
						}
					}
				} else if r := fact.Review; r != nil {
					report.Reviews++
					if r.Conflict {
						report.ExcludedConflicts++
					}
					if !r.SawStarted || !r.SawCompleted {
						report.IncompleteReviews++
					}
					g := groups["action"][r.Action]
					if g == nil {
						g = &Distribution{Value: r.Action, Totals: newTotals()}
						groups["action"][r.Action] = g
					}
					g.Reviews++
				}
				if o.Details && (kind > token.Kind || kind == token.Kind && fact.Sequence > token.Sequence) {
					if len(report.Details) < report.Limit {
						report.Details = append(report.Details, detail(fact))
						last = pageToken{Session: id, Window: report.Window, Kind: kind, Sequence: fact.Sequence, Snapshot: &snapshot}
					} else {
						more = true
					}
				}
			}
			if len(facts) < 500 {
				break
			}
		}
	}
	if groupGap && report.Anomalies[domain.GuardianCoverageGap] < math.MaxInt64 {
		report.Anomalies[domain.GuardianCoverageGap]++
	}
	if report.Reviews > 0 {
		report.Capability.ReviewAvailability = "observed"
	}
	for _, v := range []string{"below_25", "25_to_75", "75_to_95", "at_least_95", "unknown"} {
		if groups["bucket"][v] == nil {
			groups["bucket"][v] = &Distribution{Value: v, Totals: newTotals()}
		}
	}
	report.Buckets = distributions(groups["bucket"])
	report.ByModel = distributions(groups["model"])
	report.ByVersion = distributions(groups["version"])
	report.ByAction = distributions(groups["action"])
	if o.Details {
		report.ByThread = distributions(groups["thread"])
	}
	report.Reconciliation.RequestSum = report.Totals.Input
	if more {
		raw, _ := json.Marshal(last)
		report.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return report, nil
}

// Purge deletes new observation data and advances its replay fence.
func (s *QueryService) Purge(ctx context.Context, id domain.SessionID) error {
	_, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return apierr.NotFound("SESSION_NOT_FOUND", "Session not found.")
	}
	return s.store.PurgeGuardian(ctx, id, s.now().UTC())
}
func distributions(groups map[string]*Distribution) []Distribution {
	out := make([]Distribution, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}
func newTotals() Totals {
	return Totals{Known: map[string]int64{}, Unknown: map[string]int64{}, Overflow: []string{}}
}
func (t *Totals) add(v domain.GuardianTokens) {
	var uncached *int64
	if v.Input != nil && v.Cached != nil {
		n := *v.Input - *v.Cached
		uncached = &n
	}
	fields := []struct {
		name  string
		value *int64
		sum   **int64
	}{{"input", v.Input, &t.Input}, {"cached", v.Cached, &t.Cached}, {"uncached", uncached, &t.Uncached}, {"cache_write", v.CacheWrite, &t.CacheWrite}, {"output", v.Output, &t.Output}, {"reasoning", v.Reasoning, &t.Reasoning}, {"total", v.Total, &t.Total}}
	for _, f := range fields {
		if f.value == nil {
			t.Unknown[f.name]++
			continue
		}
		t.Known[f.name]++
		overflow := false
		for _, name := range t.Overflow {
			if name == f.name {
				overflow = true
			}
		}
		if overflow {
			continue
		}
		if *f.sum == nil {
			n := int64(0)
			*f.sum = &n
		}
		if **f.sum > math.MaxInt64-*f.value {
			*f.sum = nil
			t.Overflow = append(t.Overflow, f.name)
		} else {
			**f.sum += *f.value
		}
	}
}
func optional(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
func detail(f domain.GuardianFact) Detail {
	d := Detail{Kind: f.Kind, Availability: map[string]string{}, Reconciliation: "unmatched", UnavailableReason: "upstream_not_exposed"}
	if r := f.Request; r != nil {
		d.NativeEpoch = r.EpochID
		d.ThreadID = r.ThreadID
		d.TurnID = r.TurnID
		d.ResponseID = optional(r.ResponseID)
		d.RootTurnID = optional(r.RootTurnID)
		d.ModelID = optional(r.ModelID)
		d.CLIVersion = optional(r.CLIVersion)
		d.ProviderAt = &r.ProviderAt
		d.FirstSeenAt = r.FirstSeenAt
		d.Tokens = &Counters{r.Tokens.Input, r.Tokens.Cached, r.Tokens.CacheWrite, r.Tokens.Output, r.Tokens.Reasoning, r.Tokens.Total}
		d.Availability["review_id"] = "unsupported"
		d.Availability["action"] = "unsupported"
		d.Availability["model_id"] = "missing_event"
		if r.ModelID != "" {
			d.Availability["model_id"] = "observed"
			d.ModelSource = optional("turn_context")
		}
		for k, v := range map[string]*int64{"input_tokens": r.Tokens.Input, "cached_input_tokens": r.Tokens.Cached, "cache_write_input_tokens": r.Tokens.CacheWrite, "output_tokens": r.Tokens.Output, "reasoning_output_tokens": r.Tokens.Reasoning, "total_tokens": r.Tokens.Total} {
			d.Availability[k] = "missing_event"
			if v != nil {
				d.Availability[k] = "observed"
			}
		}
		if r.Conflict {
			d.Reconciliation = "conflict"
			d.UnavailableReason = "source_conflict"
		}
	} else if r := f.Review; r != nil {
		d.NativeEpoch = r.EpochID
		d.ThreadID = r.ParentThreadID
		d.TurnID = r.ParentTurnID
		d.ReviewID = optional(r.ReviewID)
		d.TargetItemID = r.TargetItemID
		d.Action = &r.Action
		d.Status = &r.Status
		d.DecisionSource = optional(r.DecisionSource)
		d.StartedAt = r.StartedAt
		d.CompletedAt = r.CompletedAt
		d.FirstSeenAt = r.FirstSeenAt
		d.Availability["guardian_link"] = "unsupported"
		d.Availability["target_item_id"] = "observed"
		d.Availability["started_event"] = "missing_event"
		d.Availability["completed_event"] = "missing_event"
		if r.SawStarted {
			d.Availability["started_event"] = "observed"
		}
		if r.SawCompleted {
			d.Availability["completed_event"] = "observed"
		}
		for k, v := range map[string]*time.Time{"started_at": r.StartedAt, "completed_at": r.CompletedAt} {
			d.Availability[k] = "missing_event"
			if v != nil {
				d.Availability[k] = "observed"
			}
		}
		d.Availability["action"] = "observed"
		if r.Action == "unknown" {
			d.Availability["action"] = "unsupported"
		}
		if r.Conflict {
			d.Reconciliation = "conflict"
			d.UnavailableReason = "source_conflict"
		}
	}
	return d
}
