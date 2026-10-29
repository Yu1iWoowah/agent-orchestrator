package domain

import "time"

// SessionAgentSettings is the durable preference and its settings-only revision.
type SessionAgentSettings struct {
	Harness                AgentHarness  `json:"-"`
	AccountID              string        `json:"-"`
	OwnerGeneration        string        `json:"-"`
	ProviderConversationID string        `json:"-"`
	BranchID               string        `json:"-"`
	IsTerminated           bool          `json:"-"`
	SessionID              SessionID     `json:"sessionId"`
	Settings               AgentSettings `json:"settings"`
	Revision               int64         `json:"revision"`
}

// AgentSettingsPhase is a durable operation checkpoint.
type AgentSettingsPhase string

// Agent settings operation checkpoints.
const (
	AgentSettingsPending          AgentSettingsPhase = "pending"
	AgentSettingsApplied          AgentSettingsPhase = "applied"
	AgentSettingsCancelled        AgentSettingsPhase = "cancelled"
	AgentSettingsFailed           AgentSettingsPhase = "failed"
	AgentSettingsRecoveryRequired AgentSettingsPhase = "recovery_required"
)

// AgentSettingsActorKind distinguishes verified agent attribution from local input.
type AgentSettingsActorKind string

// Actor kinds accepted by the settings audit.
const (
	AgentSettingsActorLocal        AgentSettingsActorKind = "local_operator_unverified"
	AgentSettingsActorOrchestrator AgentSettingsActorKind = "orchestrator"
)

// AgentSettingsActor records attribution only. Authorization belongs to the
// service boundary, not this durable non-secret record.
type AgentSettingsActor struct {
	Kind      AgentSettingsActorKind `json:"kind"`
	SessionID SessionID              `json:"sessionId,omitempty"`
}

// AgentSettingsOperation contains no credentials or provider error text.
type AgentSettingsOperation struct {
	ID                     string                     `json:"id"`
	SessionID              SessionID                  `json:"sessionId"`
	IdempotencyKey         string                     `json:"-"`
	BaseRevision           int64                      `json:"baseRevision"`
	Source                 AgentSettings              `json:"source"`
	Target                 AgentSettings              `json:"target"`
	AccountID              string                     `json:"accountId,omitempty"`
	ControllerGeneration   string                     `json:"controllerGeneration"`
	Actor                  AgentSettingsActor         `json:"actor"`
	Phase                  AgentSettingsPhase         `json:"phase" enum:"pending,applied,cancelled,failed,recovery_required"`
	CreatedAt              time.Time                  `json:"createdAt"`
	UpdatedAt              time.Time                  `json:"updatedAt"`
	ApplyMode              AgentSettingsApplyMode     `json:"applyMode" enum:"after-turn,restart"`
	Checkpoint             AgentSettingsCheckpoint    `json:"checkpoint" enum:"queued,applying_model,applying_effort,applying_fast,verifying,verified,rolling_back,restored"`
	Version                int64                      `json:"version"`
	ErrorCategory          AgentSettingsErrorCategory `json:"errorCategory,omitempty" enum:"conflict,cancelled,provider,storage,validation,recovery_required"`
	ErrorCode              string                     `json:"errorCode,omitempty"`
	RequestID              string                     `json:"requestId,omitempty"`
	ProviderConversationID string                     `json:"providerConversationId,omitempty"`
	BranchID               string                     `json:"branchId,omitempty"`
	StartedAt              *time.Time                 `json:"startedAt,omitempty"`
	FinishedAt             *time.Time                 `json:"finishedAt,omitempty"`
	Recovery               *AgentSettingsRecovery     `json:"recovery,omitempty"`
	Execution              *AgentSettingsExecution    `json:"-"`
	Authority              AgentSettingsAuthority     `json:"-"`
	RequestFingerprint     string                     `json:"-"`
}

// AgentSettingsRecovery pins a replacement executor and its durable restoration
// target. It does not replace the original operation's source, target or actor.
type AgentSettingsRecovery struct {
	Owner              AgentSettingsOwner      `json:"owner"`
	Revision           int64                   `json:"revision"`
	Settings           AgentSettings           `json:"settings"`
	ClaimedAt          time.Time               `json:"claimedAt"`
	PreviousCheckpoint AgentSettingsCheckpoint `json:"previousCheckpoint"`
}

// CanSupersedeLocally limits menu coalescing to unstarted, unverified-local
// intent. Read-only preparation is allowed; possible provider effects are not.
func (op AgentSettingsOperation) CanSupersedeLocally() bool {
	return op.Actor.Kind == AgentSettingsActorLocal && op.Actor.SessionID == "" &&
		op.Authority == (AgentSettingsAuthority{}) && op.Phase == AgentSettingsPending &&
		op.Checkpoint == AgentSettingsQueued && op.StartedAt == nil && op.Recovery == nil
}
