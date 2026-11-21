package usage

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Cohort is an explicit session list and the original UTC half-open interval.
type Cohort struct {
	SessionIDs []string `json:"sessionIds"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	start, end time.Time
}

// NewCohort parses query input once; there are no stored labels or task mappings.
func NewCohort(ids []string, from, to string) (Cohort, error) {
	out := Cohort{SessionIDs: append([]string(nil), ids...), From: from, To: to}
	seen := map[string]bool{}
	if len(ids) == 0 {
		return out, fmt.Errorf("an explicit session list is required")
	}
	for _, id := range ids {
		if id == "" || strings.TrimSpace(id) != id || seen[id] {
			return out, fmt.Errorf("session ids must be nonempty, unique and contain no surrounding whitespace")
		}
		seen[id] = true
	}
	var err error
	out.start, err = time.Parse(time.RFC3339Nano, from)
	if err != nil {
		return out, fmt.Errorf("from must be a UTC RFC3339 timestamp: %w", err)
	}
	out.end, err = time.Parse(time.RFC3339Nano, to)
	if err != nil {
		return out, fmt.Errorf("to must be a UTC RFC3339 timestamp: %w", err)
	}
	_, fromOffset := out.start.Zone()
	_, toOffset := out.end.Zone()
	if fromOffset != 0 || toOffset != 0 || !out.start.Before(out.end) {
		return out, fmt.Errorf("a nonempty UTC half-open interval is required")
	}
	return out, nil
}

// TokenTotals measures recorded vectors, with null for any unknown counter.
type TokenTotals struct {
	EventCount          int64  `json:"eventCount"`
	IncompleteEvents    int64  `json:"incompleteEvents"`
	InputTokens         *int64 `json:"inputTokens"`
	CachedInputTokens   *int64 `json:"cachedInputTokens"`
	UncachedInputTokens *int64 `json:"uncachedInputTokens"`
	OutputTokens        *int64 `json:"outputTokens"`
}

// BaselineSource separates root, child and unknown provenance without new groups.
type BaselineSource struct {
	Execution        *ExecutionSource `json:"execution,omitempty"`
	MeasurementKinds []string         `json:"measurementKinds"`
	BindingID        int64            `json:"bindingId"`
	Harness          string           `json:"harness"`
	NativeRootID     string           `json:"nativeRootId"`
	Scope            string           `json:"scope"`
	Source           Source           `json:"source"`
	Recorded         TokenTotals      `json:"recorded"`
	Undated          TokenTotals      `json:"undated"`
}

// BaselineSession retains query-time metadata and discovery/error facts.
type BaselineSession struct {
	AuxiliaryRecorded TokenTotals      `json:"auxiliaryRecorded"`
	AuxiliaryUndated  TokenTotals      `json:"auxiliaryUndated"`
	Consistency       string           `json:"consistency"`
	SessionID         string           `json:"sessionId"`
	DataRoot          string           `json:"dataRoot"`
	DataRootStatus    string           `json:"dataRootStatus"`
	ProjectID         string           `json:"projectId"`
	Role              string           `json:"role"`
	Mode              string           `json:"mode"`
	AccountID         *string          `json:"accountId"`
	MetadataAsOf      *time.Time       `json:"metadataAsOf"`
	SourceAsOf        *time.Time       `json:"sourceAsOf"`
	ThroughID         int64            `json:"throughId"`
	ScanStatus        string           `json:"scanStatus"`
	Reason            string           `json:"reason,omitempty"`
	Bindings          []Binding        `json:"bindings"`
	Sources           []BaselineSource `json:"sources"`
	Recorded          TokenTotals      `json:"recorded"`
	Undated           TokenTotals      `json:"undated"`
}

// BaselineReport is a disposable canonical cohort, never a complete usage claim.
type BaselineReport struct {
	AuxiliaryRecorded TokenTotals `json:"auxiliaryRecorded"`
	AuxiliaryUndated  TokenTotals `json:"auxiliaryUndated"`
	Cohort
	Source      string            `json:"source"`
	Status      string            `json:"status"`
	Reason      string            `json:"reason,omitempty"`
	CaptureRate *float64          `json:"captureRate"`
	Coverage    string            `json:"coverage"`
	Recorded    TokenTotals       `json:"recorded"`
	Undated     TokenTotals       `json:"undated"`
	Sessions    []BaselineSession `json:"sessions"`
}

// Baseline consumes the caller's ordered C1 scans. No native/external inputs are
// parsed, and raw provider buckets never replace the neutral ledger vector.
func Baseline(query Cohort, snapshots []Snapshot) (BaselineReport, error) {
	out := BaselineReport{Cohort: query, Source: "daemon_ledger", Status: "complete", Coverage: "unknown_no_native_event_denominator", Sessions: []BaselineSession{}}
	available := 0
	for i, id := range query.SessionIDs {
		row := BaselineSession{SessionID: id, ScanStatus: "unavailable", Reason: "scan_missing", Bindings: []Binding{}, Sources: []BaselineSource{}}
		if i >= len(snapshots) || snapshots[i].SessionID != id {
			out.Status = "partial"
			out.Sessions = append(out.Sessions, row)
			continue
		}
		s := snapshots[i]
		if s.Source != "daemon_ledger" {
			out.Status = "unavailable"
			if out.Recorded.EventCount+out.Undated.EventCount > 0 {
				out.Status = "partial"
			}
			out.Reason = "noncanonical_source"
			return out, fmt.Errorf("session %s: noncanonical_source", id)
		}
		row.DataRoot, row.DataRootStatus = s.DataRoot, s.DataRootStatus
		row.Consistency = metadataOrUnknown(s.Consistency)
		row.ProjectID, row.Role, row.Mode = metadataOrUnknown(s.ProjectID), metadataOrUnknown(s.Role), metadataOrUnknown(s.Mode)
		row.AccountID, row.MetadataAsOf, row.SourceAsOf = s.AccountID, observedTime(s.MetadataAsOf), observedTime(s.SourceAsOf)
		row.ThroughID, row.ScanStatus, row.Reason, row.Bindings = s.ThroughID, s.ScanStatus, s.Reason, s.Bindings
		if s.ScanStatus != "complete" {
			out.Status = "partial"
		}
		if s.ScanStatus == "complete" || len(s.Events) > 0 {
			available++
		}
		type sourceKey struct{ binding, source int64 }
		indices := map[sourceKey]int{}
		for _, b := range s.Bindings {
			for _, source := range b.Sources {
				indices[sourceKey{b.ID, source.ID}] = len(row.Sources)
				scope := "unknown"
				if source.Kind == "claude_main" || source.Kind == "claude_subagent" || source.Kind == "codex_rollout" {
					if b.NativeRootID != "" && source.NativeSessionID != "" {
						scope = "root"
						if source.SubagentID != "" || source.Kind == "claude_subagent" || source.NativeSessionID != b.NativeRootID {
							scope = "child"
						}
					}
				}
				row.Sources = append(row.Sources, BaselineSource{BindingID: b.ID, Harness: b.Harness, NativeRootID: b.NativeRootID, Scope: scope, Source: source})
			}
		}
		for _, e := range s.Events {
			if e.CreatedAt != nil && (e.CreatedAt.Before(query.start) || !e.CreatedAt.Before(query.end)) {
				continue
			}
			key := sourceKey{e.BindingID, e.UsageSourceID}
			if e.Execution != nil {
				if e.Execution.ID <= 0 || e.Execution.ProjectID != s.ProjectID || e.Execution.SessionID != id || e.BindingID != 0 || e.UsageSourceID != 0 {
					return out, fmt.Errorf("conflicting execution source identity")
				}
				// A negative internal index cannot collide with native positive IDs.
				key = sourceKey{0, -e.Execution.ID}
			}
			index, ok := indices[key]
			if !ok {
				index = len(row.Sources)
				indices[key] = index
				source := BaselineSource{BindingID: e.BindingID, Scope: "unknown", Source: Source{ID: e.UsageSourceID}}
				if e.Execution != nil {
					source.Scope = "auxiliary"
					source.Execution = e.Execution
					source.Source = Source{ID: e.Execution.ID, Kind: "hcg_execution"}
				}
				row.Sources = append(row.Sources, source)
			}
			kind := metadataOrUnknown(e.MeasurementKind)
			if !slices.Contains(row.Sources[index].MeasurementKinds, kind) {
				row.Sources[index].MeasurementKinds = append(row.Sources[index].MeasurementKinds, kind)
			}
			totals := []*TokenTotals{&out.Recorded, &row.Recorded, &row.Sources[index].Recorded}
			if e.CreatedAt == nil {
				totals = []*TokenTotals{&out.Undated, &row.Undated, &row.Sources[index].Undated}
			}
			if e.Execution != nil {
				totals = []*TokenTotals{&out.AuxiliaryRecorded, &row.AuxiliaryRecorded, &row.Sources[index].Recorded}
				if e.CreatedAt == nil {
					totals = []*TokenTotals{&out.AuxiliaryUndated, &row.AuxiliaryUndated, &row.Sources[index].Undated}
				}
			}
			// add replaces counter pointers, so copies retain the accepted prefix.
			// Commit all levels only after every counter has been checked.
			next := make([]TokenTotals, len(totals))
			for i, total := range totals {
				next[i] = *total
				if err := next[i].add(e); err != nil {
					out.Status = "partial"
					out.Reason = err.Error()
					out.Sessions = append(out.Sessions, row)
					return out, err
				}
			}
			for i, total := range totals {
				*total = next[i]
			}
		}
		out.Sessions = append(out.Sessions, row)
	}
	if available == 0 {
		out.Status = "unavailable"
	}
	return out, nil
}

func (t *TokenTotals) add(e Event) error {
	first := t.EventCount == 0
	t.EventCount++
	incomplete := false
	for _, metric := range []struct {
		sum   **int64
		value *int64
	}{{&t.InputTokens, e.InputTokens}, {&t.CachedInputTokens, e.CachedInputTokens}, {&t.UncachedInputTokens, e.UncachedInputTokens}, {&t.OutputTokens, e.OutputTokens}} {
		if metric.value == nil {
			incomplete = true
			*metric.sum = nil
			continue
		}
		if *metric.value < 0 {
			*metric.sum = nil
			return fmt.Errorf("negative token counter")
		}
		if !first && *metric.sum == nil {
			continue
		}
		var value int64
		if !first {
			value = **metric.sum
		}
		if *metric.value > math.MaxInt64-value {
			*metric.sum = nil
			return fmt.Errorf("token sum exceeds int64")
		}
		value += *metric.value
		*metric.sum = &value
	}
	if incomplete {
		t.IncompleteEvents++
	}
	return nil
}

func metadataOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
func observedTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}
