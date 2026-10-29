package domain

import (
	"errors"
	"time"
)

// Durable question lookup and compare-and-swap errors.
var (
	ErrAttentionQuestionForbidden = errors.New("only the active same-project orchestrator may forward this report")
	ErrAttentionQuestionNotFound  = errors.New("question not found in this session")
	ErrAttentionQuestionConflict  = errors.New("question or idempotency payload changed")
)

// AttentionQuestionCreate carries caller input; source ownership is derived from the session.
type AttentionQuestionCreate struct {
	// ActorCapability is transient request proof; ActorVerifier is set only by
	// the service after validation and fenced again inside the store transaction.
	ActorCapability string `json:"-"`
	ActorVerifier   string `json:"-"`
	ID              string
	SourceReportID  string
	OriginSessionID SessionID
	ClientKey       string
	Audience        string
	Kind            AttentionQuestionKind
	Payload         []byte
	ExpiresAt       *time.Time
	SupersedesID    string
}

// AttentionQuestionRecord is the durable source, lifecycle, read state and optional answer.
type AttentionQuestionRecord struct {
	ID              string                   `json:"id"`
	SourceReportID  string                   `json:"sourceReportId,omitempty"`
	ProjectID       ProjectID                `json:"projectId"`
	OriginSessionID SessionID                `json:"originSessionId"`
	OriginHarness   string                   `json:"originHarness"`
	Audience        string                   `json:"audience" enum:"user"`
	Kind            AttentionQuestionKind    `json:"kind" enum:"question,authorization"`
	ClientKey       string                   `json:"clientKey"`
	Payload         AttentionQuestionPayload `json:"payload"`
	State           string                   `json:"state" enum:"open,answered,cancelled,expired"`
	Version         int64                    `json:"version"`
	Sequence        int64                    `json:"sequence"`
	SupersedesID    string                   `json:"supersedesId,omitempty"`
	CreatedAt       time.Time                `json:"createdAt"`
	UpdatedAt       time.Time                `json:"updatedAt"`
	ExpiresAt       *time.Time               `json:"expiresAt,omitempty"`
	ClosedAt        *time.Time               `json:"closedAt,omitempty"`
	ClosedBy        string                   `json:"closedBy,omitempty"`
	CloseReason     string                   `json:"closeReason,omitempty"`
	Read            bool                     `json:"read"`
	Answer          *AttentionAnswerReceipt  `json:"answer,omitempty"`
}

// AttentionAnswerReceipt distinguishes durable answer storage from semantic delivery.
type AttentionAnswerReceipt struct {
	ClientAnswerID  string                    `json:"clientAnswerId"`
	QuestionVersion int64                     `json:"questionVersion"`
	Answers         []AttentionQuestionAnswer `json:"answers"`
	SavedAt         time.Time                 `json:"savedAt"`
	DeliveryState   string                    `json:"deliveryState" enum:"queued,delivered,failed"`
	DeliveredAt     *time.Time                `json:"deliveredAt,omitempty"`
	LastError       string                    `json:"lastError,omitempty"`
}

// AttentionQuestionQuery selects a bounded session-scoped lifecycle page.
type AttentionQuestionQuery struct {
	State          string
	BeforeSequence int64
	Limit          int64
}

// AttentionQuestionPage includes complete pending counts independent of its page cursor.
type AttentionQuestionPage struct {
	Items              []AttentionQuestionRecord `json:"items"`
	Open               int64                     `json:"open"`
	Unread             int64                     `json:"unread"`
	History            int64                     `json:"history"`
	HasMoreBefore      bool                      `json:"hasMoreBefore"`
	NextBeforeSequence int64                     `json:"nextBeforeSequence"`
}

// AttentionAnswerDelivery is a saved answer awaiting its original agent acceptance.
type AttentionAnswerDelivery struct {
	QuestionID      string
	OriginSessionID SessionID
	OriginHarness   string
	ClientMessageID string
	Message         string
}
