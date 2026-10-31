package domain

import "time"

// ReviewerSettingsOwner is a reviewer execution, never the worker's runtime.
// Its account and native identity are read server-side before revision CAS.
type ReviewerSettingsOwner struct {
	ReviewID   string `json:"reviewId"`
	AccountID  string `json:"accountId"`
	Generation string `json:"generation"`
	NativeID   string `json:"nativeId"`
}

// ReviewerAgentSettingsState keeps legacy unknown preferences distinct from
// an explicit provider default. A selection is for the next review pass.
type ReviewerAgentSettingsState struct {
	Review         Review
	Owner          ReviewerSettingsOwner
	Revision       int64
	Settings       *AgentSettings
	HasRunningPass bool
}

// ReviewerAgentSettingsOperation records a committed next-pass preference.
// It does not claim that the live provider has already applied that selection.
type ReviewerAgentSettingsOperation struct {
	ID                 string                 `json:"id"`
	Owner              ReviewerSettingsOwner  `json:"owner"`
	BaseRevision       int64                  `json:"baseRevision"`
	Revision           int64                  `json:"revision"`
	Phase              string                 `json:"phase"`
	ApplyMode          string                 `json:"applyMode"`
	Source             *AgentSettings         `json:"source,omitempty"`
	Target             AgentSettings          `json:"target"`
	Actor              AgentSettingsActor     `json:"actor"`
	Authority          AgentSettingsAuthority `json:"authority,omitempty"`
	PolicyRevision     string                 `json:"policyRevision"`
	CatalogFingerprint string                 `json:"catalogFingerprint"`
	RequestID          string                 `json:"requestId"`
	IdempotencyKey     string                 `json:"idempotencyKey"`
	RequestFingerprint string                 `json:"requestFingerprint"`
	CreatedAt          time.Time              `json:"createdAt"`
}
