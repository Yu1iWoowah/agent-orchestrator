package domain

import "time"

// ProjectAttentionQuery separates human decisions from coordination reports.
type ProjectAttentionQuery struct {
	Audience string
	Cursor   string
	Limit    int
}

// ProjectAttentionItem identifies an original source; it cannot resolve provider consent.
type ProjectAttentionItem struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind" enum:"agent_question,provider_approval,provider_input,worker_report"`
	SessionID      SessionID `json:"sessionId"`
	SessionName    string    `json:"sessionName"`
	Title          string    `json:"title"`
	Revision       int64     `json:"revision"`
	CreatedAt      time.Time `json:"createdAt"`
	Read           bool      `json:"read"`
	QuestionID     string    `json:"questionId,omitempty"`
	ReportID       string    `json:"reportId,omitempty"`
	ReportState    string    `json:"reportState,omitempty"`
	DeliveryState  string    `json:"deliveryState,omitempty"`
	AnswerState    string    `json:"answerState,omitempty" enum:"unknown"`
	OriginHarness  string    `json:"originHarness,omitempty"`
	BranchID       string    `json:"branchId,omitempty"`
	SourceBranchID string    `json:"sourceBranchId,omitempty"`
	RequestID      string    `json:"requestId,omitempty"`
	ConversationID string    `json:"conversationId,omitempty"`
	ActivityID     string    `json:"activityId,omitempty"`
}

// ProjectAttentionPage counts all sources before applying the bounded page cursor.
type ProjectAttentionPage struct {
	Items      []ProjectAttentionItem `json:"items"`
	Open       int64                  `json:"open"`
	Unread     int64                  `json:"unread"`
	NextCursor string                 `json:"nextCursor,omitempty"`
}
