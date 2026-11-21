package usage

import (
	"testing"
	"time"
)

func TestCurrentBands(t *testing.T) {
	for _, tc := range []struct {
		tokens int64
		want   string
	}{{399999, "below_400k"}, {400000, "handoff_400k"}, {449999, "handoff_400k"}, {450000, "urgent_450k"}} {
		if got := currentBand(&tc.tokens); got != tc.want {
			t.Fatalf("tokens=%d band=%s want=%s", tc.tokens, got, tc.want)
		}
	}
	if currentBand(nil) != "unknown" {
		t.Fatal("unknown input was treated as below threshold")
	}
}

func TestCurrentDoesNotInferLatestRequest(t *testing.T) {
	at := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	input := int64(999999)
	snapshot := Snapshot{Source: "daemon_ledger", ScanStatus: "complete", Page: Page{SessionID: "s", CurrentNativeID: "root", SourceAsOf: at, Bindings: []Binding{{ID: 1, SessionID: "s", Harness: "codex", NativeRootID: "root", State: "active", Sources: []Source{{ID: 1, Kind: "codex_rollout", NativeSessionID: "root", State: "active"}, {ID: 2, Kind: "codex_rollout", NativeSessionID: "child", SubagentID: "child", State: "active"}}}}, Events: []Event{{ID: 1, BindingID: 1, UsageSourceID: 1, InputTokens: &input, CreatedAt: &at, ProviderUsage: map[string]any{"last_token_usage": map[string]any{"input_tokens": float64(999999)}}}, {ID: 99, BindingID: 1, UsageSourceID: 2, InputTokens: &input, CreatedAt: &at}}}}
	got := Current(snapshot)
	if got.Metric != "last_observed_prompt_input_tokens" || got.Status != "unknown" || got.Tokens != nil || got.Band != "unknown" || got.SourceEventID != nil || got.EventAt != nil || got.Age != nil || got.Reason != "latest_request_unverified" {
		t.Fatalf("current=%+v", got)
	}
	if got.SourceRef == nil || got.SourceRef.UsageSourceID != 1 || got.SyncedAt == nil {
		t.Fatalf("source metadata=%+v", got)
	}
	for _, harness := range []string{"claude-code", "codex"} {
		t.Run("unverified_"+harness, func(t *testing.T) {
			s := snapshot
			s.Bindings = append([]Binding(nil), snapshot.Bindings...)
			s.Bindings[0].Sources = append([]Source(nil), snapshot.Bindings[0].Sources...)
			s.Events = append([]Event(nil), snapshot.Events...)
			s.Bindings[0].Harness = harness
			if harness == "claude-code" {
				s.Bindings[0].Sources[0].Kind = "claude_main"
				s.Events[0].ProviderUsage = map[string]any{"input_tokens": float64(10), "cache_creation_input_tokens": float64(20), "cache_read_input_tokens": float64(70), "output_tokens": float64(5)}
			} else {
				s.Events[0].ProviderUsage = map[string]any{"total_token_usage": map[string]any{"input_tokens": float64(999999)}, "model_context_window": float64(999999)}
			}
			out := Current(s)
			if out.Tokens != nil || out.SourceEventID != nil || out.Band != "unknown" || out.Status != "unknown" {
				t.Fatalf("unverified current=%+v", out)
			}
		})
	}

	for _, tc := range []struct {
		name, want string
		change     func(*Snapshot)
	}{
		{"scan", "scan_incomplete", func(s *Snapshot) { s.ScanStatus = "partial" }},
		{"external", "noncanonical_source", func(s *Snapshot) { s.Source = "external_analysis" }},
		{"native", "native_identity_unknown", func(s *Snapshot) { s.CurrentNativeID = "" }},
		{"binding", "current_binding_unavailable", func(s *Snapshot) { s.CurrentNativeID = "other" }},
		{"source error", "current_source_incomplete", func(s *Snapshot) { s.Bindings[0].Sources[0].State = "error" }},
		{"anomaly", "current_source_anomaly", func(s *Snapshot) { s.Bindings[0].Sources[0].AnomalyCount = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot
			s.Bindings = append([]Binding(nil), snapshot.Bindings...)
			s.Bindings[0].Sources = append([]Source(nil), snapshot.Bindings[0].Sources...)
			tc.change(&s)
			out := Current(s)
			if out.Status != "unknown" || out.Tokens != nil || out.Band != "unknown" || out.Reason != tc.want {
				t.Fatalf("current=%+v", out)
			}
		})
	}
}
