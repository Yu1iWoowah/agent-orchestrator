package domain

import "time"

// ChatDeliveryDisposition records delivery evidence independently of turn outcome.
type ChatDeliveryDisposition string

// Delivery dispositions distinguish AO intake from provider acknowledgement.
const (
	ChatDeliveryReserved ChatDeliveryDisposition = "dispatch_reserved"
	ChatDeliveryNotSent  ChatDeliveryDisposition = "known_not_sent"
	ChatDeliveryUnknown  ChatDeliveryDisposition = "outcome_unknown"
	ChatDeliveryAccepted ChatDeliveryDisposition = "accepted"
)

// ChatDeliveryReceipt contains only correlated, bounded protocol facts.
// A locally allocated turn ID, frame write or missing journal entry is no proof.
type ChatDeliveryReceipt struct {
	ProgressSequence  int64                   `json:"progressSequence,omitempty" minimum:"0"`
	AttemptID         string                  `json:"attemptId"`
	ProviderTurnID    string                  `json:"providerTurnId,omitempty"`
	ProviderRequestID string                  `json:"providerRequestId,omitempty"`
	ProviderEpoch     string                  `json:"providerEpoch,omitempty"`
	Disposition       ChatDeliveryDisposition `json:"disposition" enum:"dispatch_reserved,known_not_sent,outcome_unknown,accepted"`
	Source            string                  `json:"source" enum:"provider_response,provider_event,not_forwarded,local_not_forwarded,incomplete"`
	Complete          bool                    `json:"complete"`
	Progress          string                  `json:"progress,omitempty" enum:"running,approval,tool,background"`
	ExecutionSettled  bool                    `json:"executionSettled"`
	CustodyFlushed    bool                    `json:"custodyFlushed"`
	ObservedAt        time.Time               `json:"observedAt"`
}

// ChatDeliveryInputRef preserves the receipt's historical attribution while a
// qualified successor may dispatch it. It is captured by storage, never a client.
type ChatDeliveryInputRef struct {
	TurnID    string    `json:"turnId"`
	BranchID  string    `json:"branchId"`
	SessionID SessionID `json:"sessionId"`
}

// ChatDeliveryAttempt reserves all members before any provider I/O. PR3 can
// extend its evidence and settlement without changing turn or held storage.
type ChatDeliveryAttempt struct {
	InputRefs        []ChatDeliveryInputRef  `json:"inputRefs"`
	ID               string                  `json:"id"`
	SessionID        SessionID               `json:"sessionId"`
	ConversationID   string                  `json:"conversationId"`
	Owner            AgentSettingsOwner      `json:"owner"`
	Revision         int64                   `json:"revision"`
	TurnIDs          []string                `json:"turnIds"`
	Version          int64                   `json:"version"`
	Disposition      ChatDeliveryDisposition `json:"disposition" enum:"dispatch_reserved,known_not_sent,outcome_unknown,accepted"`
	EvidenceLevel    int                     `json:"evidenceLevel" minimum:"0" maximum:"2"`
	PreparedTurnID   string                  `json:"preparedTurnId,omitempty"`
	ExecutionSettled bool                    `json:"executionSettled"`
	Receipt          *ChatDeliveryReceipt    `json:"receipt,omitempty"`
	CreatedAt        time.Time               `json:"createdAt"`
	UpdatedAt        time.Time               `json:"updatedAt"`
	LastProgressAt   time.Time               `json:"lastProgressAt"`
	BlockedSince     *time.Time              `json:"blockedSince,omitempty"`
	AttentionID      string                  `json:"attentionId,omitempty"`
	ErrorCode        string                  `json:"errorCode,omitempty"`
	NextCheckAt      *time.Time              `json:"nextCheckAt,omitempty"`
}

// Accepts prevents downgrading accepted work or treating incomplete absence as
// not-sent. Unknown can only converge using an exact positive receipt.
func (a ChatDeliveryAttempt) Accepts(r ChatDeliveryReceipt) bool {
	if r.AttemptID != a.ID || r.ObservedAt.IsZero() {
		return false
	}
	if a.Receipt != nil {
		old := a.Receipt
		if old.ProviderEpoch != "" && r.ProviderEpoch != old.ProviderEpoch || old.ProviderRequestID != "" && r.ProviderRequestID != old.ProviderRequestID || old.ProviderTurnID != "" && r.ProviderTurnID != old.ProviderTurnID {
			return false
		}
	}
	if a.Disposition == ChatDeliveryAccepted && r.Disposition != ChatDeliveryAccepted || a.Disposition == ChatDeliveryNotSent && r.Disposition != ChatDeliveryNotSent {
		return false
	}
	switch r.Disposition {
	case ChatDeliveryAccepted:
		return r.Complete && r.ProviderEpoch != "" && r.ProviderRequestID != "" && (r.Source == "provider_response" || r.Source == "provider_event")
	case ChatDeliveryNotSent:
		if r.Source == "local_not_forwarded" {
			return r.Complete && r.ProviderRequestID == "local:"+a.ID
		}
		return r.Complete && r.ProviderEpoch != "" && r.ProviderRequestID != "" && r.Source == "not_forwarded"
	case ChatDeliveryUnknown:
		return r.Source == "incomplete" && !r.Complete
	}
	return false
}
