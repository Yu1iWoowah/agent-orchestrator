package usage

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

type eventsStore interface {
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	SessionAgentSettings(context.Context, domain.SessionID) (domain.SessionAgentSettings, error)
	GetUsageEventsThroughID(context.Context, domain.SessionID) (int64, error)
	ListUsageEventsPage(context.Context, domain.SessionID, int64, int64, int64) ([]domain.UsageLedgerEvent, error)
	ListUsageBindingsForSession(context.Context, domain.SessionID) ([]domain.UsageBindingRecord, error)
	ListUsageSourcesForBinding(context.Context, int64) ([]domain.UsageSourceRecord, error)
}

// EventsReader reads the canonical ledger without collecting or writing usage.
type EventsReader struct{ store eventsStore }

// NewEventsReader constructs the read-only events service.
func NewEventsReader(s eventsStore) *EventsReader { return &EventsReader{store: s} }

// Events fixes an insertion bound on the first page. Existing rows may change.
func (r *EventsReader) Events(ctx context.Context, id domain.SessionID, after int64, through *int64, limit int64) (domain.UsageEventsPage, error) {
	out := domain.UsageEventsPage{SessionID: id, Events: []domain.UsageEventView{}, Bindings: []domain.UsageBindingView{}, NextAfterID: after, Consistency: "insertion_bound_only; existing rows and metadata may change"}
	if after < 0 || limit < 1 || limit > 1000 || (through != nil && (*through < 0 || after > *through)) || (through == nil && after != 0) {
		return out, apierr.Invalid("INVALID_USAGE_PAGE", "afterId requires throughId; IDs must be nonnegative and ordered; limit must be 1..1000", nil)
	}
	session, ok, err := r.store.GetSession(ctx, id)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	out.MetadataAsOf = time.Now().UTC()
	out.ProjectID, out.Role, out.Mode = session.ProjectID, string(session.Kind), string(session.Mode)
	settings, err := r.store.SessionAgentSettings(ctx, id)
	if err != nil {
		return out, err
	}
	out.CurrentNativeID = settings.ProviderConversationID
	// Current preferences are annotations, never historical event ownership.
	if settings.Harness == session.Harness && settings.Mode == session.Mode && settings.AccountID != "" {
		account := settings.AccountID
		out.AccountID = &account
	}
	maximum, err := r.store.GetUsageEventsThroughID(ctx, id)
	if err != nil {
		return out, err
	}
	out.ThroughID = maximum
	if through != nil {
		if *through > maximum {
			return out, apierr.Invalid("INVALID_USAGE_PAGE", "throughId exceeds existing event upper bound", nil)
		}
		out.ThroughID = *through
	}
	rows, err := r.store.ListUsageEventsPage(ctx, id, after, out.ThroughID, limit+1)
	if err != nil {
		return out, err
	}
	out.HasMore = int64(len(rows)) > limit
	if out.HasMore {
		rows = rows[:limit]
	}
	for _, row := range rows {
		e := domain.UsageEventView{ID: row.ID, BindingID: row.BindingID, UsageSourceID: row.UsageSourceID, SourceEventKey: row.SourceEventKey,
			ProviderID: string(row.ProviderID), ModelID: row.ModelID, MeasurementKind: string(row.MeasurementKind), InputTokens: row.Tokens.InputTokens,
			CachedInputTokens: row.Tokens.CachedInputTokens, UncachedInputTokens: row.Tokens.UncachedInputTokens, OutputTokens: row.Tokens.OutputTokens}
		if !row.CreatedAt.IsZero() {
			at := row.CreatedAt
			e.CreatedAt = &at
		}
		e.ProviderUsage, e.UsageProjectionStatus = projectProviderUsage(row.ProviderUsageJSON)
		out.Events = append(out.Events, e)
		out.NextAfterID = row.ID
	}
	bindings, err := r.store.ListUsageBindingsForSession(ctx, id)
	if err != nil {
		return out, err
	}
	for _, b := range bindings {
		v := domain.UsageBindingView{ID: b.ID, SessionID: b.SessionID, Harness: string(b.Harness), NativeRootID: b.NativeRootID, State: string(b.State), LastErrorCode: b.LastErrorCode, Sources: []domain.UsageSourceView{}}
		sources, err := r.store.ListUsageSourcesForBinding(ctx, b.ID)
		if err != nil {
			return out, err
		}
		for _, s := range sources {
			v.Sources = append(v.Sources, domain.UsageSourceView{ID: s.ID, Kind: string(s.Kind), NativeSessionID: s.NativeSessionID, SubagentID: s.SubagentID, Generation: s.Generation, State: string(s.State), LastErrorCode: s.LastErrorCode, AnomalyCount: s.AnomalyCount})
		}
		out.Bindings = append(out.Bindings, v)
	}
	out.SourceAsOf = time.Now().UTC()
	return out, nil
}

// Only safe nonnegative integers are public; absent values never become zero.
func projectProviderUsage(raw string) (map[string]any, string) {
	out := map[string]any{}
	if raw == "" {
		return out, "missing"
	}
	var object map[string]json.RawMessage
	if len(raw) > 65536 || json.Unmarshal([]byte(raw), &object) != nil || object == nil {
		return out, "invalid"
	}
	status := "complete"
	scalar := map[string]bool{"input_tokens": true, "cache_creation_input_tokens": true, "cache_read_input_tokens": true, "output_tokens": true, "model_context_window": true, "ao_derived_cache_write_input_tokens": true}
	buckets := map[string]bool{"input_tokens": true, "cached_input_tokens": true, "cache_write_input_tokens": true, "output_tokens": true, "reasoning_output_tokens": true, "total_tokens": true}
	for k, v := range object {
		if k == "last_token_usage" || k == "total_token_usage" {
			var nested map[string]json.RawMessage
			if json.Unmarshal(v, &nested) != nil || nested == nil {
				status = "partial"
				continue
			}
			values := map[string]int64{}
			for name, value := range nested {
				n, err := strconv.ParseInt(string(value), 10, 64)
				if !buckets[name] || err != nil || n < 0 || n > 9007199254740991 {
					status = "partial"
					continue
				}
				values[name] = n
			}
			if len(values) > 0 {
				out[k] = values
			}
		} else {
			n, err := strconv.ParseInt(string(v), 10, 64)
			if !scalar[k] || err != nil || n < 0 || n > 9007199254740991 {
				status = "partial"
				continue
			}
			out[k] = n
		}
	}
	if len(out) == 0 && status == "complete" {
		status = "missing"
	}
	return out, status
}
