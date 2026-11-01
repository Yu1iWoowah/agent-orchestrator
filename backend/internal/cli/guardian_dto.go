package cli

import "time"

// Hand-mirrored safe daemon DTOs. Never decode Chat activity detail here.
type guardianReportDTO struct {
	SchemaVersion     int                       `json:"schema_version"`
	Enabled           bool                      `json:"enabled"`
	Window            guardianWindowDTO         `json:"window"`
	Capability        guardianCapabilityDTO     `json:"capability"`
	Coverage          string                    `json:"coverage"`
	Anomalies         map[string]int64          `json:"anomalies"`
	Requests          int64                     `json:"requests"`
	Reviews           int64                     `json:"reviews"`
	ExactReviewLinks  int64                     `json:"exact_review_links"`
	UnmatchedRequests int64                     `json:"unmatched_requests"`
	ExcludedConflicts int64                     `json:"excluded_conflicts"`
	IncompleteReviews int64                     `json:"incomplete_reviews"`
	Totals            guardianTotalsDTO         `json:"totals"`
	Buckets           []guardianDistributionDTO `json:"cache_buckets"`
	ByModel           []guardianDistributionDTO `json:"by_model"`
	ByVersion         []guardianDistributionDTO `json:"by_version"`
	ByAction          []guardianDistributionDTO `json:"by_action"`
	ByThread          []guardianDistributionDTO `json:"by_guardian_thread,omitempty"`
	Reconciliation    guardianReconciliationDTO `json:"ledger_reconciliation"`
	ReuseState        string                    `json:"reuse_state"`
	TranscriptMode    *string                   `json:"transcript_mode"`
	ResetReason       *string                   `json:"reset_reason"`
	UnavailableReason string                    `json:"unavailable_reason"`
	Details           []guardianDetailDTO       `json:"details,omitempty"`
	NextCursor        string                    `json:"next_cursor,omitempty"`
	Limit             int                       `json:"limit"`
}

type guardianWindowDTO struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}
type guardianCapabilityDTO struct {
	Provider           string `json:"provider"`
	ReviewEvents       string `json:"review_events"`
	ReviewAvailability string `json:"review_availability"`
	DirectReviewLinks  string `json:"direct_review_links"`
	ContextDecisions   string `json:"context_decisions"`
}

// guardianTotalsDTO sums known values only. Unknown counts and overflow are explicit;
// a missing counter is never treated as an observed zero.
type guardianTotalsDTO struct {
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
type guardianDistributionDTO struct {
	Value             string            `json:"value"`
	Requests          int64             `json:"requests"`
	Reviews           int64             `json:"reviews"`
	Unmatched         int64             `json:"unmatched_requests"`
	ExcludedConflicts int64             `json:"excluded_conflicts"`
	Totals            guardianTotalsDTO `json:"totals"`
}
type guardianReconciliationDTO struct {
	State      string `json:"state"`
	RequestSum *int64 `json:"request_input_sum"`
	LedgerSum  *int64 `json:"ledger_input_sum"`
	Difference *int64 `json:"difference"`
	Reason     string `json:"unavailable_reason"`
}

// guardianDetailDTO contains native IDs only on explicit request. Link fields stay null
// until an upstream direct-evidence contract is separately implemented.
type guardianDetailDTO struct {
	Kind              string               `json:"kind"`
	NativeEpoch       int64                `json:"native_epoch"`
	ThreadID          string               `json:"thread_id"`
	TurnID            string               `json:"turn_id"`
	ReviewID          *string              `json:"review_id"`
	ResponseID        *string              `json:"response_id"`
	RootTurnID        *string              `json:"root_turn_id"`
	TargetItemID      *string              `json:"target_item_id"`
	Action            *string              `json:"action"`
	Status            *string              `json:"status"`
	DecisionSource    *string              `json:"decision_source"`
	ModelID           *string              `json:"model_id"`
	ModelSource       *string              `json:"model_source"`
	CLIVersion        *string              `json:"cli_version"`
	ProviderAt        *time.Time           `json:"provider_at"`
	StartedAt         *time.Time           `json:"started_at"`
	CompletedAt       *time.Time           `json:"completed_at"`
	FirstSeenAt       time.Time            `json:"first_seen_at"`
	Tokens            *guardianCountersDTO `json:"tokens"`
	Availability      map[string]string    `json:"availability"`
	Reconciliation    string               `json:"reconciliation"`
	EvidenceKind      *string              `json:"evidence_kind"`
	UnavailableReason string               `json:"unavailable_reason"`
}
type guardianCountersDTO struct {
	Input      *int64 `json:"input_tokens"`
	Cached     *int64 `json:"cached_input_tokens"`
	CacheWrite *int64 `json:"cache_write_input_tokens"`
	Output     *int64 `json:"output_tokens"`
	Reasoning  *int64 `json:"reasoning_output_tokens"`
	Total      *int64 `json:"total_tokens"`
}
