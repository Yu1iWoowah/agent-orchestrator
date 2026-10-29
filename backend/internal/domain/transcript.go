package domain

import "time"

// TranscriptSourceSnapshot is a read-only inventory, not the visible timeline.
// Fingerprint covers the raw SQL rows, including identities omitted from UI JSON.
type TranscriptSourceSnapshot struct {
	Session             SessionRecord
	Fingerprint         string
	CutoffSequence      int64
	ConversationCutoffs map[string]int64
	Branches            []ConversationBranch
	Turns               []ConversationTurn
	Inputs              []TranscriptTurnInput
	Messages            []ConversationMessage
	Activities          []ConversationActivity
	NativeThreads       []ConversationNativeThreadEvidence
	Terminal            []TranscriptTerminalTurn
	Transitions         []SessionInterfaceTransition
	NativeSessions      []TranscriptNativeSession
	Bindings            []UsageBindingRecord
	Sources             []UsageSourceRecord
}

// TranscriptSourceCutoff pins AO projection membership while later queued
// intake remains visible and durable for the successor. It does not pin native
// bytes, which have their own verified per-epoch boundaries.
type TranscriptSourceCutoff struct {
	ConversationCutoffs map[string]int64 `json:"conversationCutoffs"`
}

// TranscriptTerminalTurn retains the native identity omitted by summary views.
type TranscriptTerminalTurn struct {
	ID          int64
	Harness     AgentHarness
	NativeID    string
	Request     string
	Response    string
	RequestedAt time.Time
	RespondedAt *time.Time
}

// TranscriptNativeSession identifies one retained provider/config epoch.
type TranscriptNativeSession struct {
	Harness        AgentHarness
	NativeID       string
	ConfigDir      string
	TranscriptPath string
	CreatedAt      time.Time
}

// TranscriptTurnInput retains upstream dispatch/member facts hidden by UI DTOs.
type TranscriptTurnInput struct {
	TurnID            string
	ProviderInputText string
	PromotedToTurnID  string
	BatchedInput      bool
	Reserved          bool
}

// TranscriptEpochCutoff is the immutable native boundary of one export attempt.
type TranscriptEpochCutoff struct {
	SHA256   string `json:"sha256,omitempty"`
	NativeID string `json:"nativeId"`
	Schema   string `json:"schema"`
	Bytes    int64  `json:"bytes"`
	LeafID   string `json:"leafId"`
}
