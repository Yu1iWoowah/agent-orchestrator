package domain

import "errors"

// ErrAttentionChanged rejects a stale or unavailable request revision.
var ErrAttentionChanged = errors.New("attention request changed or is no longer available")

// ConversationAttentionQuery selects one bounded page independently of Chat's
// loaded transcript. ActivityID is used for a fresh source lookup before focus.
type ConversationAttentionQuery struct {
	State          string
	BeforeSequence int64
	Limit          int64
	ActivityID     string
}

// ConversationAttentionItem combines a provider fact with independent read state.
type ConversationAttentionItem struct {
	Activity   ConversationActivity
	BranchID   string
	Actionable bool
	Read       bool
}

// ConversationAttentionCounts covers the full visible source set, not one page.
type ConversationAttentionCounts struct {
	Open    int64
	Unread  int64
	History int64
}

// ConversationAttentionPage is a bounded view of a single branch snapshot.
type ConversationAttentionPage struct {
	ConversationID     string
	ActiveBranchID     string
	Items              []ConversationAttentionItem
	Counts             ConversationAttentionCounts
	HasMoreBefore      bool
	NextBeforeSequence int64
}
