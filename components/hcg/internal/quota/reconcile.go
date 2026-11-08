package quota

import (
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/usage"
)

type Point struct {
	SampledAt time.Time  `json:"sampledAt"`
	FetchedAt *time.Time `json:"fetchedAt"`
	Status    string     `json:"status"`
	Window
}
type EventRef struct {
	SessionID string     `json:"sessionId"`
	EventID   int64      `json:"eventId"`
	SourceID  int64      `json:"sourceId"`
	EventAt   *time.Time `json:"eventAt"`
}
type Segment struct {
	AccountID             string                `json:"accountId"`
	Provider              string                `json:"provider"`
	WindowID              string                `json:"windowId"`
	Start                 *Point                `json:"start"`
	End                   *Point                `json:"end"`
	Status                string                `json:"status"`
	Reason                string                `json:"reason"`
	Attribution           string                `json:"attribution"`
	DeltaPercentagePoints *float64              `json:"deltaPercentagePoints"`
	Ledger                *usage.BaselineReport `json:"ledger"`
	EventRefs             []EventRef            `json:"eventRefs"`
}
type Reconciliation struct {
	usage.Cohort
	Source     string               `json:"source"`
	DataRoot   string               `json:"dataRoot"`
	Status     string               `json:"status"`
	MaxAge     string               `json:"maxAge"`
	MaxGap     string               `json:"maxGap"`
	Unassigned usage.BaselineReport `json:"unassigned"`
	Segments   []Segment            `json:"segments"`
}
type windowKey struct{ provider, account, window string }

// Reconcile compares adjacent sampling attempts, never skipping errors or
// missing windows. C1 has no historical account mapping: tokens stay unassigned.
func Reconcile(query usage.Cohort, samples []Sample, scans []usage.Snapshot, root string, maxAge, maxGap time.Duration) (Reconciliation, error) {
	out := Reconciliation{Cohort: query, Source: "account_cli", DataRoot: root, Status: "complete", MaxAge: maxAge.String(), MaxGap: maxGap.String(), Segments: []Segment{}}
	if maxAge <= 0 || maxGap <= 0 {
		return out, fmt.Errorf("explicit positive max-age and max-gap required")
	}
	// Validate input even when used outside the command boundary.
	if _, err := usage.NewCohort(query.SessionIDs, query.From, query.To); err != nil {
		return out, err
	}
	start, _ := time.Parse(time.RFC3339Nano, query.From)
	end, _ := time.Parse(time.RFC3339Nano, query.To)
	for _, s := range scans {
		if s.DataRoot != root {
			return out, fmt.Errorf("ledger namespace mismatch")
		}
	}
	var err error
	out.Unassigned, err = usage.Baseline(query, scans)
	if err != nil {
		return out, err
	}
	if out.Unassigned.Status != "complete" {
		out.Status = "partial"
	}
	samples = append([]Sample(nil), samples...)
	for _, s := range samples {
		if s.DataRoot != root || s.SampledAt.IsZero() {
			return out, fmt.Errorf("invalid quota namespace or time")
		}
	}
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].SampledAt.Before(samples[j].SampledAt) })
	previous := map[windowKey]Point{}
	var previousAt time.Time
	for _, sample := range samples {
		current := points(sample)
		keys := map[windowKey]bool{}
		for k := range previous {
			keys[k] = true
		}
		for k := range current {
			keys[k] = true
		}
		ordered := make([]windowKey, 0, len(keys))
		for k := range keys {
			ordered = append(ordered, k)
		}
		sort.Slice(ordered, func(i, j int) bool {
			a, b := ordered[i], ordered[j]
			if a.provider != b.provider {
				return a.provider < b.provider
			}
			if a.account != b.account {
				return a.account < b.account
			}
			return a.window < b.window
		})
		if !sample.SampledAt.Before(start) && sample.SampledAt.Before(end) {
			for _, key := range ordered {
				seg := Segment{AccountID: key.account, Provider: key.provider, WindowID: key.window, Status: "not_comparable", Reason: "missing_sample", Attribution: "unassigned", EventRefs: []EventRef{}}
				a, aok := previous[key]
				b, bok := current[key]
				if aok {
					seg.Start = &a
				}
				if bok {
					seg.End = &b
				}
				if aok && bok {
					seg.Reason = comparePoints(a, b, sample.SampledAt.Sub(previousAt), maxAge, maxGap)
					if a.FetchedAt != nil && b.FetchedAt != nil && a.FetchedAt.Before(*b.FetchedAt) && !a.FetchedAt.Before(start) && !b.FetchedAt.After(end) {
						interval, _ := usage.NewCohort(query.SessionIDs, a.FetchedAt.UTC().Format(time.RFC3339Nano), b.FetchedAt.UTC().Format(time.RFC3339Nano))
						ledger, scanErr := usage.Baseline(interval, scans)
						if scanErr != nil {
							return out, scanErr
						}
						seg.Ledger = &ledger
						for _, s := range scans {
							for _, e := range s.Events {
								if e.CreatedAt != nil && !e.CreatedAt.Before(*a.FetchedAt) && e.CreatedAt.Before(*b.FetchedAt) {
									seg.EventRefs = append(seg.EventRefs, EventRef{s.SessionID, e.ID, e.UsageSourceID, e.CreatedAt})
								}
							}
						}
					}
					if seg.Reason == "" {
						if seg.Ledger == nil {
							seg.Reason = "outside_cohort"
						} else {
							delta := *b.UsedPercent - *a.UsedPercent
							seg.DeltaPercentagePoints = &delta
							switch {
							case delta < 0:
								seg.Reason = "reset_or_rolling"
							case seg.Ledger.Status != "complete" || !healthy(scans):
								seg.Reason = "ledger_quality"
							case seg.Ledger.Undated.EventCount > 0:
								seg.Reason = "undated_events"
							case seg.Ledger.Recorded.EventCount > 0:
								seg.Status, seg.Reason = "observed_together", "temporal_overlap_only_account_unassigned"
							case delta > 0:
								seg.Status, seg.Reason = "unaccounted_usage_candidate", "no_recorded_events"
							default:
								seg.Reason = "no_net_increase"
							}
						}
					}
				}
				out.Segments = append(out.Segments, seg)
			}
		}
		previous, previousAt = current, sample.SampledAt
	}
	if len(out.Segments) == 0 {
		out.Status = "unavailable"
	}
	return out, nil
}
func points(s Sample) map[windowKey]Point {
	out := map[windowKey]Point{}
	if s.Status != "success" {
		return out
	}
	for _, a := range s.Accounts {
		windows := a.Windows
		if len(windows) == 0 {
			windows = []Window{{}}
		}
		for _, w := range windows {
			out[windowKey{a.Provider, a.ID, w.ID}] = Point{SampledAt: s.SampledAt, FetchedAt: a.FetchedAt, Status: a.Status, Window: w}
		}
	}
	return out
}
func comparePoints(a, b Point, gap, maxAge, maxGap time.Duration) string {
	if a.Status != "available" || b.Status != "available" || a.UsedPercent == nil || b.UsedPercent == nil || a.FetchedAt == nil || b.FetchedAt == nil {
		return "quota_unavailable"
	}
	if a.FetchedAt.Equal(*b.FetchedAt) {
		return "repeated_fetched_at"
	}
	if gap <= 0 || gap > maxGap {
		return "sampling_gap"
	}
	for _, p := range []Point{a, b} {
		age := p.SampledAt.Sub(*p.FetchedAt)
		if age < 0 || age > maxAge {
			return "stale_or_future"
		}
	}
	if !a.FetchedAt.Before(*b.FetchedAt) {
		return "stale_or_future"
	}
	if a.ResetsAt == nil || b.ResetsAt == nil {
		return "reset_missing"
	}
	if !a.ResetsAt.Equal(*b.ResetsAt) || !b.FetchedAt.Before(*a.ResetsAt) {
		return "reset_or_rolling"
	}
	return ""
}
func healthy(scans []usage.Snapshot) bool {
	for _, s := range scans {
		if s.Source != "daemon_ledger" || s.ScanStatus != "complete" {
			return false
		}
		for _, b := range s.Bindings {
			if b.LastErrorCode != "" {
				return false
			}
			for _, src := range b.Sources {
				if src.LastErrorCode != "" || src.AnomalyCount > 0 || src.State == "error" || src.State == "unsupported" {
					return false
				}
			}
		}
	}
	return true
}
