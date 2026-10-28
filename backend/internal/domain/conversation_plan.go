package domain

import "time"

// ConversationPlanVersion is one visible turn's last saved plan. An empty step
// list is an explicit clear; no version at all means no plan has been reported.
type ConversationPlanVersion struct {
	ConversationID   string
	BranchID         string
	TurnID           string
	SourceActivityID string
	Sequence         int64
	Revision         int64
	UpdatedAt        time.Time
	Plan             ConversationPlan
}
