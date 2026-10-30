package domain

import "time"

// ReplacementIntake is a live, read-only queue inventory, distinct from the
// immutable export cutoff. Reading it never claims or acknowledges delivery.
type ReplacementIntake struct {
	OperationID        string                     `json:"operationId"`
	ProjectID          ProjectID                  `json:"projectId"`
	SourceSessionID    SessionID                  `json:"sourceSessionId"`
	SuccessorSessionID SessionID                  `json:"successorSessionId,omitempty"`
	Phase              ReplacementPhase           `json:"phase"`
	IntakeRevision     int64                      `json:"intakeRevision"`
	Messages           []ReplacementIntakeMessage `json:"messages"`
	Reports            []ReplacementIntakeReport  `json:"reports"`
}

// ReplacementIntakeMessage preserves the canonical receipt and complete prompt
// payload. NativeDelivery remains unconfirmed even if a provider was reserved.
type ReplacementIntakeMessage struct {
	ID                  string        `json:"id"`
	ConversationID      string        `json:"conversationId"`
	TurnID              string        `json:"turnId"`
	Sequence            int64         `json:"sequence"`
	ClientMessageID     string        `json:"clientMessageId,omitempty"`
	Text                string        `json:"text"`
	DeliveryContentJSON string        `json:"deliveryContentJson,omitempty"`
	Origin              MessageOrigin `json:"origin"`
	SenderSessionID     string        `json:"senderSessionId,omitempty"`
	SenderProjectID     string        `json:"senderProjectId,omitempty"`
	SenderDisplayName   string        `json:"senderDisplayName,omitempty"`
	RequestedAt         time.Time     `json:"requestedAt"`
	// ProviderReserved includes durable reserve/dispatch markers, never a delivery ACK.
	ProviderReserved bool   `json:"providerReserved"`
	NativeDelivery   string `json:"nativeDelivery"`
}

// ReplacementIntakeReport omits outbox claim tokens and internal scheduling.
type ReplacementIntakeReport struct {
	ID            string                    `json:"id"`
	SessionID     SessionID                 `json:"sessionId"`
	State         ReportState               `json:"state"`
	Note          string                    `json:"note"`
	Message       string                    `json:"message"`
	CreatedAt     time.Time                 `json:"createdAt"`
	DeliveryState ReportDeliveryState       `json:"deliveryState"`
	RepeatCount   int64                     `json:"repeatCount"`
	Outputs       []ReplacementIntakeOutput `json:"outputs"`
}

// ReplacementIntakeOutput retains a report's source/artifact references.
type ReplacementIntakeOutput struct {
	Kind      ReportOutputKind `json:"kind"`
	Reference string           `json:"reference"`
	Label     string           `json:"label,omitempty"`
}
