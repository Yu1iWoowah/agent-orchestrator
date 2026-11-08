package usage

import "time"

// CurrentReading is the handoff metric; unknown values are never zero-filled.
type CurrentReading struct {
	SessionID     string            `json:"session_id"`
	Metric        string            `json:"metric"`
	Tokens        *int64            `json:"tokens"`
	Status        string            `json:"status"`
	Reason        string            `json:"reason"`
	SourceEventID *int64            `json:"source_event_id"`
	SourceRef     *CurrentSourceRef `json:"source_ref"`
	EventAt       *time.Time        `json:"event_at"`
	SyncedAt      *time.Time        `json:"synced_at"`
	Age           *float64          `json:"age"`
	Band          string            `json:"band"`
}

// CurrentSourceRef identifies observed main-source metadata, not a latest event.
type CurrentSourceRef struct {
	BindingID     int64  `json:"binding_id"`
	UsageSourceID int64  `json:"usage_source_id"`
	Harness       string `json:"harness"`
	NativeRootID  string `json:"native_root_id"`
}

// Current keeps the metric unknown until Q2 certifies ordering and completeness
// for a concrete source/version. C1 alone has no such evidence. Neither event id,
// timestamp, a complete single usage nor neutral throughput proves "latest".
func Current(s Snapshot) CurrentReading {
	out := CurrentReading{SessionID: s.SessionID, Metric: "last_observed_prompt_input_tokens", Status: "unknown", Band: currentBand(nil)}
	if s.ScanStatus != "complete" {
		out.Reason = "scan_incomplete"
		if s.Reason != "" {
			out.Reason += ": " + s.Reason
		}
		return out
	}
	if !s.SourceAsOf.IsZero() {
		at := s.SourceAsOf
		out.SyncedAt = &at
	}
	if s.Source != "daemon_ledger" {
		out.Reason = "noncanonical_source"
		return out
	}
	if s.CurrentNativeID == "" {
		out.Reason = "native_identity_unknown"
		return out
	}
	var matches []Binding
	for _, b := range s.Bindings {
		if b.SessionID == s.SessionID && b.NativeRootID == s.CurrentNativeID {
			matches = append(matches, b)
		}
	}
	if len(matches) != 1 {
		out.Reason = "current_binding_unavailable"
		return out
	}
	binding := matches[0]
	if (binding.State != "active" && binding.State != "complete") || binding.LastErrorCode != "" {
		out.Reason = "current_binding_incomplete"
		return out
	}
	var main []Source
	for _, source := range binding.Sources {
		if source.NativeSessionID != binding.NativeRootID || source.SubagentID != "" || source.Kind == "claude_subagent" {
			continue
		}
		if source.Kind != "claude_main" && source.Kind != "codex_rollout" {
			continue
		}
		if source.AnomalyCount > 0 {
			out.Reason = "current_source_anomaly"
			return out
		}
		if (source.State != "active" && source.State != "complete") || source.LastErrorCode != "" {
			out.Reason = "current_source_incomplete"
			return out
		}
		main = append(main, source)
	}
	if len(main) != 1 {
		out.Reason = "current_source_unavailable"
		return out
	}
	out.SourceRef = &CurrentSourceRef{BindingID: binding.ID, UsageSourceID: main[0].ID, Harness: binding.Harness, NativeRootID: binding.NativeRootID}
	out.Reason = "latest_request_unverified"
	return out
}

func currentBand(tokens *int64) string {
	switch {
	case tokens == nil || *tokens < 0:
		return "unknown"
	case *tokens < 400000:
		return "below_400k"
	case *tokens < 450000:
		return "handoff_400k"
	default:
		return "urgent_450k"
	}
}
