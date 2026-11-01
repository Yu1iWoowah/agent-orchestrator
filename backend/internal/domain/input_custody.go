package domain

import (
	"errors"
	"time"
)

// ErrInputCustodyConflict rejects stale, mismatched or unqualified actions.
var ErrInputCustodyConflict = errors.New("input custody action conflicts with durable state")

// InputCustodyVersion identifies the exact ledger custodian, not a provider
// acceptance or permission to dispatch. Owner is a coordinator-owned reference.
type InputCustodyVersion struct {
	TurnID   string
	Owner    string
	Revision int64
}

// InputHoldVersion identifies one hold producer and its expected revision.
type InputHoldVersion struct {
	Key     string
	Owner   string
	Version int64
}

// InputHold retains the reason and active state of an independent hold layer.
type InputHold struct {
	InputHoldVersion
	Reason string
	Active bool
}

// InputCustody retains the original input location even when its custodian
// changes. The canonical turn and message remain the only payload records.
type InputCustody struct {
	InputCustodyVersion
	OriginalState          TurnState
	OriginalConversationID string
	OriginalBranchID       string
	OriginalSessionID      SessionID
	OriginalGeneration     string
	OriginalSequence       int64
	RequestedAt            time.Time
	Holds                  []InputHold
}

// InputCustodyReceipt acknowledges only the durable storage action. A repeat
// action key returns the same revision without replaying the mutation.
type InputCustodyReceipt struct {
	ActionKey string
	TurnID    string
	Revision  int64
}
