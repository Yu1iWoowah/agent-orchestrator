package domain

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"time"
)

// ReplacementNoticeState describes delivery evidence, never reading or a D ACK.
type ReplacementNoticeState string

// Durable notice delivery states; none establishes reading or a directive ACK.
const (
	ReplacementNoticePending   ReplacementNoticeState = "pending"
	ReplacementNoticeAccepted  ReplacementNoticeState = "accepted"
	ReplacementNoticeUncertain ReplacementNoticeState = "uncertain"
	ReplacementNoticeSkipped   ReplacementNoticeState = "skipped"
)

// ReplacementNoticeReceipt is the durable delivery projection. Attempts also
// retain append-only snapshots; updating this row never rewrites that history.
type ReplacementNoticeReceipt struct {
	OperationID    string                 `json:"operationId"`
	SessionID      SessionID              `json:"sessionId"`
	ContactID      string                 `json:"contactId"`
	State          ReplacementNoticeState `json:"state"`
	Owner          SessionControllerOwner `json:"owner"`
	Mode           SessionMode            `json:"mode"`
	AttemptID      string                 `json:"attemptId,omitempty"`
	PayloadHash    string                 `json:"payloadHash"`
	ConversationID string                 `json:"conversationId,omitempty"`
	MessageID      string                 `json:"messageId,omitempty"`
	TurnID         string                 `json:"turnId,omitempty"`
	EvidenceKind   string                 `json:"evidenceKind,omitempty"`
	ErrorCode      string                 `json:"errorCode,omitempty"`
	CreatedAt      time.Time              `json:"createdAt"`
	AttemptedAt    *time.Time             `json:"attemptedAt,omitempty"`
	// OutcomeRecordedAt is private attempt bookkeeping, not delivery acceptance.
	OutcomeRecordedAt *time.Time `json:"outcomeRecordedAt,omitempty"`
	SettledAt         *time.Time `json:"settledAt,omitempty"`
	Revision          int64      `json:"-"`
}

// NativeDeliveryID is an internal, per-attempt correlation, not the public
// ContactID. It must not be projected to an unauthenticated read endpoint.
func (p ReplacementNoticeReceipt) NativeDeliveryID() string {
	if p.AttemptID == "" {
		return ""
	}
	return "handoff-notice-attempt:" + p.AttemptID
}

// ReplacementNotificationCompletion seals the verified receipt set once.
type ReplacementNotificationCompletion struct {
	TargetCount   int       `json:"targetCount"`
	AcceptedCount int       `json:"acceptedCount"`
	SkippedCount  int       `json:"skippedCount"`
	ReceiptHash   string    `json:"receiptHash"`
	CompletedAt   time.Time `json:"completedAt"`
}

// ValidFor checks shape only. Store publication must verify the receipt ledger.
func (p ReplacementNotificationCompletion) ValidFor(op OrchestratorReplacement) bool {
	digest, err := hex.DecodeString(p.ReceiptHash)
	return err == nil && len(digest) == 32 && p.TargetCount > 0 && p.TargetCount == len(op.Notifications) && p.AcceptedCount >= 0 && p.SkippedCount >= 0 && p.AcceptedCount+p.SkippedCount == p.TargetCount && !p.CompletedAt.IsZero()
}

// WorkerFYI is system-authored coordination, not a directive or task handoff.
func (op OrchestratorReplacement) WorkerFYI() string {
	link := "ao://sessions/" + url.PathEscape(string(op.ProjectID)) + "/" + url.PathEscape(string(op.SuccessorSessionID))
	return fmt.Sprintf("FYI：本專案的 orchestrator 已從 %q 換成 %q。請繼續原本任務；ao report 會依 project 送給目前的 orchestrator。這不是 D，不需 ACK，不變更任務或 PR ownership。\nOperation: %s\n新 orchestrator：%s", op.SourceSessionID, op.SuccessorSessionID, op.ID, link)
}
