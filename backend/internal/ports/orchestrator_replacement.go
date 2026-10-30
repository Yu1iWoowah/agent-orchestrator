package ports

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ReplacementRetirementProof is supplied by the document service after full
// evidence verification. Check must perform bounded stability comparisons only.
// It is an internal dependency, never an HTTP-supplied assertion.
type ReplacementRetirementProof interface {
	OperationID() string
	OperationRevision() int64
	Source() domain.SessionRecord
	DocumentRevision() int64
	Check(context.Context) error
}

// ReplacementSourceObservation identifies the controller and concrete control
// turn facts. Unknown provenance never establishes successful preparation.
type ReplacementSourceObservation struct {
	Owner               domain.SessionControllerOwner
	NativeID            string
	Provenance          domain.NativeThreadProvenance
	State               ChatControllerState
	ObservedAt          time.Time
	ActiveTurn          *domain.ConversationTurn
	ControlTurn         *domain.ConversationTurn
	PendingApproval     bool
	ControlAcknowledged bool
}

// ReplacementPreparationDelivery is an acceptance receipt, not completion.
type ReplacementPreparationDelivery struct {
	TurnID    string
	Owner     domain.SessionControllerOwner
	NativeID  string
	Uncertain bool
}

// ReplacementResumeObservation certifies an operation-owned required replay.
// The verifier is an internal hash, never a raw launch capability or an API DTO.
type ReplacementResumeObservation struct {
	Owner              domain.SessionControllerOwner
	NativeID           string
	CapabilityVerifier string
	HistoryVerified    bool
	ObservedAt         time.Time
}

// ReplacementChatRuntime is the optional production bridge for controlled
// handoff turns and observations. Merely supporting ordinary Chat is insufficient.
type ReplacementChatRuntime interface {
	PreflightReplacementChat(context.Context, domain.AgentHarness, PermissionMode) error
	ObserveReplacementChatSource(context.Context, domain.OrchestratorReplacement) (ReplacementSourceObservation, error)
	EnsureReplacementChatPreparation(context.Context, domain.OrchestratorReplacement, string, bool) (ReplacementPreparationDelivery, error)
	QuiesceReplacementChatSource(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error)
	ObserveReplacementChatSuccessor(context.Context, domain.OrchestratorReplacement) (ReplacementSourceObservation, error)
	EnsureReplacementChatBootstrap(context.Context, domain.OrchestratorReplacement, string, bool) (ReplacementPreparationDelivery, error)
	QuiesceReplacementChatSuccessor(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error)
	EnsureReplacementChatProbe(context.Context, domain.OrchestratorReplacement, string, bool) (ReplacementPreparationDelivery, error)
	QueueReplacementIntake(context.Context, domain.SessionID, ChatUserMessage) (domain.ConversationTurn, error)
}

// ReplacementWorkerNoticeStore is the internal delivery authority. Attempts
// never authorize arbitrary text, targets, or generic accepted receipts.
type ReplacementWorkerNoticeStore interface {
	BeginReplacementWorkerNotice(context.Context, string, int64, domain.SessionID, domain.SessionControllerOwner, time.Time) (domain.ReplacementNoticeReceipt, bool, error)
	FinishReplacementWorkerNotice(context.Context, string, domain.SessionID, string, int64, bool, time.Time) (bool, error)
	ValidateReplacementWorkerNotice(context.Context, string, domain.SessionID, string, int64) (domain.OrchestratorReplacement, domain.SessionRecord, domain.ReplacementNoticeReceipt, error)
}

// ReplacementDormantWorkerNoticeQueue admits fixed FYI text directly to a
// dormant worker's canonical queue. It cannot wake or dispatch a provider.
type ReplacementDormantWorkerNoticeQueue interface {
	QueueDormantReplacementWorkerNotice(context.Context, string, int64, domain.SessionID, domain.SessionControllerOwner, time.Time) (bool, error)
}
