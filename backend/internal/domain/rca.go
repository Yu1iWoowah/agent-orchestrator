package domain

import "time"

// RCABucket measures an observed fragment category, never billed usage. Null
// means unobserved; a witnessed empty fragment can have a numeric zero.
type RCABucket struct {
	Category        string `json:"category"`
	Bytes           *int64 `json:"bytes"`
	EstimatedTokens *int64 `json:"estimatedTokens"`
}

// RCARecordedTurn is a numeric ordinal in an authorized source snapshot.
type RCARecordedTurn struct {
	Ordinal int64        `json:"ordinal"`
	Buckets [6]RCABucket `json:"buckets"`
}

// RCARecordedSnapshot contains only fixed categories, numbers and AO-owned
// references. Native IDs, filenames, paths and external names are never fields.
type RCARecordedSnapshot struct {
	ScopeID          string            `json:"scopeId"`
	UsageSourceID    *int64            `json:"usageSourceId"`
	BindingID        *int64            `json:"bindingId"`
	SourceGeneration *int64            `json:"sourceGeneration"`
	Association      string            `json:"association"`
	Method           string            `json:"method"`
	SourceVersion    string            `json:"sourceVersion"`
	Status           string            `json:"status"`
	Reasons          []string          `json:"reasons"`
	StartOffset      int64             `json:"startOffset"`
	EndOffset        int64             `json:"endOffset"`
	AnalyzedBytes    int64             `json:"analyzedBytes"`
	Records          int64             `json:"records"`
	Buckets          [6]RCABucket      `json:"buckets"`
	Turns            []RCARecordedTurn `json:"turns"`
	Unpaired         [6]RCABucket      `json:"unpaired"`
}

// RCARecordedState is the durable numeric projection; canonical usage is
// intentionally absent and must be reread through the existing EventsReader.
type RCARecordedState struct {
	ProjectID     ProjectID             `json:"projectId"`
	SessionID     SessionID             `json:"sessionId"`
	ScopeRevision string                `json:"scopeRevision"`
	ObservedAt    time.Time             `json:"observedAt"`
	Status        string                `json:"status"`
	Reason        string                `json:"reason"`
	Snapshots     []RCARecordedSnapshot `json:"snapshots"`
}

// RCAPolicyView reports effective RCA policy without source data.
type RCAPolicyView struct {
	Project          *bool  `json:"project"`
	Role             *bool  `json:"role"`
	Session          *bool  `json:"session"`
	Source           string `json:"source"`
	SessionOverride  string `json:"sessionOverride"`
	EffectiveEnabled bool   `json:"effectiveEnabled"`
	EmergencyStop    bool   `json:"emergencyStop"`
	ManifestRevision uint64 `json:"manifestRevision"`
}

// RCACanonicalEvent is a projection of an existing row, not another ledger.
type RCACanonicalEvent struct {
	ID                int64  `json:"id"`
	BindingID         int64  `json:"bindingId"`
	UsageSourceID     int64  `json:"usageSourceId"`
	InputTokens       *int64 `json:"inputTokens"`
	OutputTokens      *int64 `json:"outputTokens"`
	CachedInputTokens *int64 `json:"cachedInputTokens"`
}

// RCACanonicalPage is a fresh numeric view of canonical events.
type RCACanonicalPage struct {
	ThroughID    int64               `json:"throughId"`
	NextAfterID  int64               `json:"nextAfterId"`
	HasMore      bool                `json:"hasMore"`
	Events       []RCACanonicalEvent `json:"events"`
	MetadataAsOf time.Time           `json:"metadataAsOf"`
	SourceAsOf   time.Time           `json:"sourceAsOf"`
}

// RCABaseline is an independent, non-request synthetic reference.
type RCABaseline struct {
	Status          string `json:"status"`
	Method          string `json:"method"`
	Bytes           *int64 `json:"bytes"`
	EstimatedTokens *int64 `json:"estimatedTokens"`
}

// RCAComposition separates recorded estimates, baseline and canonical usage.
type RCAComposition struct {
	ProjectID          ProjectID         `json:"projectId"`
	SessionID          SessionID         `json:"sessionId"`
	Policy             RCAPolicyView     `json:"policy"`
	Status             string            `json:"status"`
	Reason             string            `json:"reason"`
	RequestComposition string            `json:"requestComposition"`
	Recorded           *RCARecordedState `json:"recorded"`
	ClaudeInline       *RCAClaudeInline  `json:"claudeInline"`
	Canonical          RCACanonicalPage  `json:"canonical"`
	Baseline           RCABaseline       `json:"baseline"`
}

// RCAQuery selects a canonical page and an optional explicit analysis.
type RCAQuery struct {
	AfterID   int64
	ThroughID *int64
	Limit     int64
	Analyze   bool
}

// RCAClaudeAttempt is an observed inline attempt, never a wire request or a
// file offset. Canonical references remain nil until an exact key chain is proven.
type RCAClaudeAttempt struct {
	Ordinal          int64        `json:"ordinal"`
	Buckets          [6]RCABucket `json:"buckets"`
	MCPBytes         *int64       `json:"mcpBytes"`
	Reasons          []string     `json:"reasons"`
	CanonicalEventID *int64       `json:"canonicalEventId"`
}

// RCAClaudeInline contains AO references and fixed numeric/quality fields only.
// Native identifiers, exporter marker, paths and body hashes are never stored.
type RCAClaudeInline struct {
	ProjectID      ProjectID          `json:"projectId"`
	SessionID      SessionID          `json:"sessionId"`
	CaptureID      string             `json:"captureId"`
	Role           string             `json:"role"`
	Requested      bool               `json:"requested"`
	Active         bool               `json:"active"`
	Reason         string             `json:"reason"`
	Coverage       string             `json:"coverage"`
	Method         string             `json:"method"`
	ObservedAt     time.Time          `json:"observedAt"`
	Attempts       []RCAClaudeAttempt `json:"attempts"`
	Unpaired       int64              `json:"unpaired"`
	DroppedBatches int64              `json:"droppedBatches"`
}
