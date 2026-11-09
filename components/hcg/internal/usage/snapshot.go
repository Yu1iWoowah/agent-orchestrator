// Package usage holds disposable numeric snapshots of the daemon ledger.
package usage

import "time"

// Event exposes only ledger identity and numeric usage.
type Event struct {
	Execution             *ExecutionSource `json:"execution,omitempty"`
	ID                    int64            `json:"id"`
	BindingID             int64            `json:"bindingId"`
	UsageSourceID         int64            `json:"usageSourceId"`
	SourceEventKey        string           `json:"sourceEventKey"`
	ProviderID            string           `json:"providerId"`
	ModelID               string           `json:"modelId"`
	MeasurementKind       string           `json:"measurementKind"`
	InputTokens           *int64           `json:"inputTokens"`
	CachedInputTokens     *int64           `json:"cachedInputTokens"`
	UncachedInputTokens   *int64           `json:"uncachedInputTokens"`
	OutputTokens          *int64           `json:"outputTokens"`
	CreatedAt             *time.Time       `json:"createdAt"`
	ProviderUsage         map[string]any   `json:"providerUsage"`
	UsageProjectionStatus string           `json:"usageProjectionStatus" enum:"complete,partial,missing,invalid"`
}

// Binding omits paths, parser state and credentials.
type Binding struct {
	ID            int64    `json:"id"`
	SessionID     string   `json:"sessionId"`
	Harness       string   `json:"harness"`
	NativeRootID  string   `json:"nativeRootId"`
	State         string   `json:"state"`
	LastErrorCode string   `json:"lastErrorCode"`
	Sources       []Source `json:"sources"`
}

// Source describes all discovered sources, including ones without events.
type Source struct {
	ID              int64  `json:"id"`
	Kind            string `json:"kind"`
	NativeSessionID string `json:"nativeSessionId"`
	SubagentID      string `json:"subagentId"`
	Generation      int64  `json:"generation"`
	State           string `json:"state"`
	LastErrorCode   string `json:"lastErrorCode"`
	AnomalyCount    int64  `json:"anomalyCount"`
}

// Page is a bounded read, not an isolated historical snapshot.
type Page struct {
	SessionID       string    `json:"sessionId"`
	Events          []Event   `json:"events"`
	Bindings        []Binding `json:"bindings"`
	ThroughID       int64     `json:"throughId"`
	NextAfterID     int64     `json:"nextAfterId"`
	HasMore         bool      `json:"hasMore"`
	SourceAsOf      time.Time `json:"sourceAsOf"`
	MetadataAsOf    time.Time `json:"metadataAsOf"`
	ProjectID       string    `json:"projectId"`
	Role            string    `json:"role"`
	Mode            string    `json:"mode"`
	AccountID       *string   `json:"accountId"`
	CurrentNativeID string    `json:"currentNativeId"`
	Consistency     string    `json:"consistency"`
}

// Snapshot belongs to one scan. It is never merged with an older result.
type Snapshot struct {
	DataRootStatus string    `json:"dataRootStatus"`
	Source         string    `json:"source"`
	DataRoot       string    `json:"dataRoot"`
	ScanStatus     string    `json:"scanStatus"`
	ScannedAt      time.Time `json:"scannedAt"`
	Reason         string    `json:"reason,omitempty"`
	Page
}

// ExecutionSource identifies auxiliary consumption independently of native sources.
type ExecutionSource struct {
	ID                    int64  `json:"id"`
	ProjectID             string `json:"projectId"`
	SessionID             string `json:"sessionId"`
	DelegationID          string `json:"delegationId"`
	AttemptID             string `json:"attemptId"`
	BillingProvider       string `json:"billingProvider"`
	ProfileID             string `json:"profileId"`
	ProfileRevision       string `json:"profileRevision"`
	ProfileDigest         string `json:"profileDigest"`
	ProjectPolicyRevision string `json:"projectPolicyRevision"`
	RequestID             string `json:"requestId"`
	ResponseID            string `json:"responseId,omitempty"`
	UnknownReason         string `json:"unknownReason,omitempty"`
}
