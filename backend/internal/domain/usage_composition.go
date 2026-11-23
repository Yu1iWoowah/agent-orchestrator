package domain

// UsageComposition is one session's per-request context composition, computed
// on demand from the provider's local session file. Nothing is stored.
type UsageComposition struct {
	SessionID SessionID `json:"sessionId"`
	// ReviewID is set for a reviewer, whose file is not in AO's usage ledger.
	ReviewID     string `json:"reviewId,omitempty"`
	NotCanonical bool   `json:"notCanonical,omitempty"`
	// Source is the native file kind read: claude_main or codex_rollout.
	Source string `json:"source"`
	// Status is unknown when no request records could be read.
	Status   string                    `json:"status" enum:"ok,unknown"`
	Reason   string                    `json:"reason,omitempty" enum:",no_source,no_request_records,source_read_failed"`
	Requests []UsageCompositionRequest `json:"requests"`
}

// UsageCompositionRequest is one model request.
type UsageCompositionRequest struct {
	Ordinal   int    `json:"ordinal"`
	Timestamp string `json:"timestamp,omitempty"`
	// InputTokens is the provider-reported input including cache reads and
	// writes; nil when the request has no usage record.
	InputTokens *int64 `json:"inputTokens"`
	// Unresolved counts message references the file could not resolve.
	Unresolved int                        `json:"unresolved,omitempty"`
	Categories []UsageCompositionCategory `json:"categories"`
}

// UsageCompositionCategory splits InputTokens by each category's share of
// bytes. Bytes nil is UNKNOWN; Tokens nil means the category is not allocated.
type UsageCompositionCategory struct {
	Category string `json:"category" enum:"system,tools,mcp_tools,injected_system,history,tool_use,tool_result,thinking,thinking_encrypted,world_state"`
	Bytes    *int64 `json:"bytes"`
	Tokens   *int64 `json:"tokens"`
	Note     string `json:"note,omitempty" enum:",unknown,encrypted_hidden,delivery_unverified"`
}
