package ports

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

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
