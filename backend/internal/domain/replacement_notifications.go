package domain

import (
	"crypto/sha256"
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

// ReplacementWorkerFYIPayload freezes the rendered version at activation.
// Keep every shipped renderer stable, including the nil legacy v0 fallback.
type ReplacementWorkerFYIPayload struct {
	Version int    `json:"version"`
	Text    string `json:"text"`
	SHA256  string `json:"sha256"`
}

func (op OrchestratorReplacement) NewWorkerFYIPayload() *ReplacementWorkerFYIPayload {
	text := op.workerFYIV0()
	sum := sha256.Sum256([]byte(text))
	return &ReplacementWorkerFYIPayload{Version: 1, Text: text, SHA256: hex.EncodeToString(sum[:])}
}

func (p ReplacementWorkerFYIPayload) ValidFor(op OrchestratorReplacement) bool {
	// New template versions must add their own renderer, never edit v0.
	if p.Version != 1 || p.Text != op.workerFYIV0() {
		return false
	}
	sum := sha256.Sum256([]byte(p.Text))
	return p.SHA256 == hex.EncodeToString(sum[:])
}

// WorkerFYI is system-authored coordination, not a directive or task handoff.
func (op OrchestratorReplacement) WorkerFYI() string {
	if op.WorkerFYIPayload != nil {
		return op.WorkerFYIPayload.Text
	}
	return op.workerFYIV0()
}

// workerFYIV0 is also the pinned renderer for legacy operations without a
// payload. Never change it when introducing another template version.
func (op OrchestratorReplacement) workerFYIV0() string {
	link := "ao://sessions/" + url.PathEscape(string(op.ProjectID)) + "/" + url.PathEscape(string(op.SuccessorSessionID))
	return fmt.Sprintf("FYI：本專案的 orchestrator 已從 %q 換成 %q。請繼續原本任務；ao report 會依 project 送給目前的 orchestrator。這不是 D，不需 ACK，不變更任務或 PR ownership。\nOperation: %s\n新 orchestrator：%s", op.SourceSessionID, op.SuccessorSessionID, op.ID, link)
}
