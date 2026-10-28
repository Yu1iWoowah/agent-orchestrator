package domain

import "errors"

var ErrAttentionChanged = errors.New("attention request changed or is no longer available")

// ConversationAttentionQuery selects one bounded page independently of Chat's
// loaded transcript. ActivityID is used for a fresh source lookup before focus.
type ConversationAttentionQuery struct {
	State          string
	BeforeSequence int64
	Limit          int64
	ActivityID     string
}

type ConversationAttentionItem struct {
	Activity   ConversationActivity
	BranchID   string
	Actionable bool
	Read       bool
}

type ConversationAttentionCounts struct {
	Open    int64
	Unread  int64
	History int64
}

type ConversationAttentionPage struct {
	ConversationID     string
	ActiveBranchID     string
	Items              []ConversationAttentionItem
	Counts             ConversationAttentionCounts
	HasMoreBefore      bool
	NextBeforeSequence int64
}
