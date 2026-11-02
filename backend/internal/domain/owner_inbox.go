package domain

import (
	"errors"
	"time"
)

// ErrOwnerInboxConflict and related errors distinguish stale plans, unresolved
// routing and missing transaction participants without claiming provider delivery.
var (
	ErrOwnerInboxConflict           = errors.New("owner inbox revision or intent conflict")
	ErrOwnerInboxInputNotBound      = errors.New("owner inbox batch belongs to another input; preserve this user input")
	ErrOwnerRoutingUnavailable      = errors.New("routing_unavailable")
	ErrOwnerInboxParticipantMissing = errors.New("owner inbox intake participant unavailable")
)

// InboxOwner is a durable authority tuple, never a derived display status.
// The disabled core does not backfill it or change legacy session projections.
type InboxOwner struct {
	ProjectID      ProjectID
	SessionID      SessionID
	Epoch          int64
	ConversationID string
	BranchID       string
	Generation     string
	PolicyRevision int64
}

// InboxSourceVersion pins a report independently of claim tokens and routing.
type InboxSourceVersion struct {
	ReportID string
	Version  int64
}

// InboxReportIntent identifies one caller-authored report, independent of retries.
type InboxReportIntent struct {
	Key    string
	Report ReportRecord
}

// InboxBatchPlan is prepared outside the transaction. TextHash pins the exact
// report summary without copying report bodies into another journal.
type InboxBatchPlan struct {
	ProjectID ProjectID
	Key       string
	Owner     InboxOwner
	Members   []InboxSourceVersion
	TextHash  string
}

// InboxInputPlan pins canonical input identity and expected S intake evidence.
type InboxInputPlan struct {
	ProjectID       ProjectID
	Key             string
	BatchKey        string
	Owner           InboxOwner
	TurnID          string
	MessageID       string
	ClientMessageID string
	// Text is filled by H from SummaryText/UserText before the S participant.
	Text        string
	SummaryText string
	// UserText is nil for direct delivery; piggyback uses the existing envelope.
	UserText *string
	// ExistingTurnID is the canonical user input for piggyback acceptance.
	// The transaction verifies its identity and text rather than appending again.
	ExistingTurnID          string
	SettingsRef             string
	SettingsOwner           AgentSettingsOwner
	SettingsRevision        int64
	RuntimeQualificationRef string
	HeadRevision            int64
	LedgerRevision          int64
	// BatchPolicyRevision is filled by H from the immutable claim plan.
	// Owner.PolicyRevision identifies the current owner admission separately.
	BatchPolicyRevision int64
	// Members is populated by H from its fixed batch before S evaluates intake.
	Members []InboxSourceVersion
	At      time.Time
}

// InboxReceipt proves only AO's durable acceptance, never provider L2 or a
// permit to perform I/O. Replays return these original references unchanged.
type InboxReceipt struct {
	ProjectID    ProjectID
	Key          string
	Stage        string
	ReportID     string
	BatchKey     string
	TurnID       string
	OwnerEpoch   int64
	SafeReason   string
	MetadataRefs []string
}

// InboxMetadataPlan scopes enlistment to immutable source versions and stage.
type InboxMetadataPlan struct {
	ProjectID ProjectID
	Stage     string
	Owner     InboxOwner
	BatchKey  string
	Sources   []InboxSourceVersion
	// PolicyRevision pins the batch summary plan; Owner carries current admission.
	PolicyRevision int64
}

// InboxIntakeReceipt is supplied by the transaction-bound S adapter. It cannot
// grant an outbound permit; S obtains that in its second transaction/lane.
type InboxIntakeReceipt struct {
	InputID    string
	ReceiptRef string
	SafeReason string
}
