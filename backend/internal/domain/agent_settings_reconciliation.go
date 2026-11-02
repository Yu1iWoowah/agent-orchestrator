package domain

import "time"

// AgentSettingsReconciliation is work progress, never a session display status
// or a replacement preference revision. No raw provider error is retained.
type AgentSettingsReconciliation struct {
	PreferenceAtStart AgentSettings `json:"preferenceAtStart"`
	RetryOfVersion    int64         `json:"retryOfVersion,omitempty"`
	RetryOfAttemptID  string        `json:"retryOfAttemptId,omitempty"`

	ID                           string             `json:"id"`
	SessionID                    SessionID          `json:"sessionId"`
	Owner                        AgentSettingsOwner `json:"owner"`
	Revision                     int64              `json:"revision"`
	OperationID                  string             `json:"operationId,omitempty"`
	OriginReplacementOperationID *string            `json:"originReplacementOperationId,omitempty"`
	ProviderEpoch                string             `json:"providerEpoch,omitempty"`
	EvidenceKind                 string             `json:"evidenceKind" enum:"unknown,provider_current,per_turn_wire"`
	ObservationRef               string             `json:"observationRef,omitempty"`
	Phase                        string             `json:"phase" enum:"observing,waiting_idle,applying,verifying,ready,blocked,recovery_required,cancelled"`
	Step                         string             `json:"step"`
	LastSuccessfulStep           string             `json:"lastSuccessfulStep,omitempty"`
	FailedStep                   string             `json:"failedStep,omitempty"`
	Version                      int64              `json:"version"`
	RetryCount                   int                `json:"retryCount"`
	ExecutorID                   string             `json:"-"`
	ActionKey                    string             `json:"actionKey,omitempty"`
	EffectPending                bool               `json:"effectPending"`
	ClaimUntil                   time.Time          `json:"claimUntil"`
	Deadline                     time.Time          `json:"deadline"`
	CreatedAt                    time.Time          `json:"createdAt"`
	LastProgressAt               time.Time          `json:"lastProgressAt"`
	FailedAt                     *time.Time         `json:"failedAt,omitempty"`
	BlockedSince                 *time.Time         `json:"blockedSince,omitempty"`
	ErrorCode                    string             `json:"errorCode,omitempty"`
	ErrorCategory                string             `json:"errorCategory,omitempty"`
	SafeCause                    string             `json:"safeCause,omitempty"`
	ProviderErrorCode            string             `json:"providerErrorCode,omitempty"`
	JSONRPCCode                  *int               `json:"jsonRpcCode,omitempty"`
	RequestID                    string             `json:"requestId,omitempty"`
	Desired                      AgentSettings      `json:"desired"`
	HistoricalApplied            *AgentSettings     `json:"historicalApplied,omitempty"`
	Observed                     *AgentSettings     `json:"observed,omitempty"`
	ObservationAvailability      string             `json:"observationAvailability" enum:"unknown,available,unsupported"`
	MissingEvidence              []string           `json:"missingEvidence"`
	RecoveryActions              []string           `json:"recoveryActions"`
}
