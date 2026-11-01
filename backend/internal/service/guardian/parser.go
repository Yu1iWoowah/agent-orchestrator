// Package guardian observes metadata-only Codex guardian facts. It never
// participates in an approval decision or writes the billing ledger.
package guardian

import (
	"encoding/json"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type header struct {
	CreatedAt time.Time
	ID        string
	Parent    string
	Version   string
	Guardian  bool
}

// parseHeader is deliberately separate from the existing spawn discovery:
// guardian parent_thread_id is top-level, not source.subagent.thread_spawn.
func parseHeader(line []byte) (header, bool) {
	var e struct {
		Timestamp time.Time `json:"timestamp"`
		Type      string    `json:"type"`
		Payload   struct {
			ID           string          `json:"id"`
			Parent       string          `json:"parent_thread_id"`
			ThreadSource string          `json:"thread_source"`
			Version      string          `json:"cli_version"`
			Source       json.RawMessage `json:"source"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &e) != nil || e.Type != "session_meta" || !domain.ValidGuardianID(e.Payload.ID) {
		return header{}, false
	}
	h := header{CreatedAt: e.Timestamp.UTC(), ID: e.Payload.ID, Parent: e.Payload.Parent, Version: e.Payload.Version, Guardian: e.Payload.ThreadSource == "guardian_review"}
	if !domain.ValidGuardianVersion(h.Version) {
		h.Version = ""
	}
	if !h.Guardian && len(e.Payload.Source) > 0 && e.Payload.Source[0] == '{' {
		var source struct {
			Subagent json.RawMessage `json:"subagent"`
		}
		if json.Unmarshal(e.Payload.Source, &source) != nil {
			return header{}, false
		}
		var spawn struct {
			Spawn struct {
				Parent string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		}
		if len(source.Subagent) > 0 && source.Subagent[0] == '"' {
			// Codex review/compact subagents have no spawn parent evidence.
			var kind string
			if json.Unmarshal(source.Subagent, &kind) != nil {
				return header{}, false
			}
		} else if len(source.Subagent) > 0 && json.Unmarshal(source.Subagent, &spawn) != nil {
			return header{}, false
		}
		if parent := spawn.Spawn.Parent; parent != "" {
			if h.Parent != "" && h.Parent != parent {
				return header{}, false
			}
			h.Parent = parent
		}
	}
	if h.Parent != "" && !domain.ValidGuardianID(h.Parent) {
		return header{}, false
	}
	if h.Guardian && h.Parent == "" {
		return header{}, false
	}
	return h, true
}

type record struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type       string `json:"type"`
		ThreadID   string `json:"thread_id"`
		TurnID     string `json:"turn_id"`
		ResponseID string `json:"response_id"`
		RootTurnID string `json:"root_turn_id"`
		Model      string `json:"model"`
		Usage      struct {
			Input      *int64 `json:"input_tokens"`
			Cached     *int64 `json:"cached_input_tokens"`
			CacheWrite *int64 `json:"cache_write_input_tokens"`
			Output     *int64 `json:"output_tokens"`
			Reasoning  *int64 `json:"reasoning_output_tokens"`
			Total      *int64 `json:"total_tokens"`
		} `json:"usage"`
	} `json:"payload"`
}

// appendRecord retains only a bounded, typed whitelist. Unknown nested JSON,
// action payloads, messages and rationale never cross this boundary.
func appendRecord(b *domain.GuardianBatch, line []byte, offset int64) {
	var r record
	if json.Unmarshal(line, &r) != nil {
		b.Anomalies[domain.GuardianInvalidMetadata]++
		return
	}
	r.Timestamp = r.Timestamp.UTC()
	if r.Type != "turn_context" && (r.Type != "event_msg" || r.Payload.Type != "token_usage_record") {
		return
	}
	if r.Timestamp.IsZero() {
		b.Anomalies[domain.GuardianUnknownTime]++
		return
	}
	if r.Timestamp.After(b.Cursor.UpdatedAt) {
		b.Anomalies[domain.GuardianInvalidMetadata]++
		return
	}
	if !b.Owner.AcceptsProviderTime(r.Timestamp, b.Cursor.UpdatedAt) {
		return
	}

	if !domain.ValidGuardianID(r.Payload.TurnID) {
		b.Anomalies[domain.GuardianInvalidMetadata]++
		return
	}
	if r.Payload.ThreadID != "" && r.Payload.ThreadID != b.Thread.ThreadID {
		b.Anomalies[domain.GuardianOwnerConflict]++
		return
	}
	if r.Type == "turn_context" {
		if !domain.ValidGuardianModel(r.Payload.Model) {
			b.Anomalies[domain.GuardianInvalidMetadata]++
			return
		}
		b.Models = append(b.Models, domain.GuardianTurnModel{TurnID: r.Payload.TurnID, ModelID: r.Payload.Model, ProviderAt: r.Timestamp})
		return
	}
	if r.Payload.ThreadID != b.Thread.ThreadID {
		b.Anomalies[domain.GuardianOwnerConflict]++
		return
	}
	if !domain.ValidGuardianID(r.Payload.ResponseID) || (r.Payload.RootTurnID != "" && !domain.ValidGuardianID(r.Payload.RootTurnID)) {
		b.Anomalies[domain.GuardianInvalidMetadata]++
		return
	}
	u := r.Payload.Usage
	tokens := domain.GuardianTokens{Input: u.Input, Cached: u.Cached, CacheWrite: u.CacheWrite, Output: u.Output, Reasoning: u.Reasoning, Total: u.Total}
	if !tokens.Valid() {
		b.Anomalies[domain.GuardianInvalidMetadata]++
		return
	}
	b.Requests = append(b.Requests, domain.GuardianRequest{ThreadID: b.Thread.ThreadID, TurnID: r.Payload.TurnID, ResponseID: r.Payload.ResponseID, RootTurnID: r.Payload.RootTurnID, Tokens: tokens, ProviderAt: r.Timestamp, SourceGeneration: b.Cursor.Generation, SourceOffset: offset})
}
