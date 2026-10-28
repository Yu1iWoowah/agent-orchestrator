package domain

import "time"

// SessionAgentSettings is the durable preference and its settings-only revision.
type SessionAgentSettings struct {
	Harness         AgentHarness  `json:"-"`
	AccountID       string        `json:"-"`
	OwnerGeneration string        `json:"-"`
	SessionID       SessionID     `json:"sessionId"`
	Settings        AgentSettings `json:"settings"`
	Revision        int64         `json:"revision"`
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
	ID                   string             `json:"id"`
	SessionID            SessionID          `json:"sessionId"`
	IdempotencyKey       string             `json:"-"`
	BaseRevision         int64              `json:"baseRevision"`
	Source               AgentSettings      `json:"source"`
	Target               AgentSettings      `json:"target"`
	AccountID            string             `json:"accountId,omitempty"`
	ControllerGeneration string             `json:"controllerGeneration"`
	Actor                AgentSettingsActor `json:"actor"`
	Phase                AgentSettingsPhase `json:"phase" enum:"pending,applied,cancelled,failed,recovery_required"`
	CreatedAt            time.Time          `json:"createdAt"`
	UpdatedAt            time.Time          `json:"updatedAt"`
}
