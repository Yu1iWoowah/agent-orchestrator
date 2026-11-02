package ports

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// InputCustodyStore is shared storage for the handoff owner coordinator (PR1)
// and the settings/delivery coordinator. It neither executes provider I/O nor
// decides whether delivery evidence authorizes release. Callers must settle the
// old controller's outbound boundary before creating a hold or moving custody.
type InputCustodyStore interface {
	InputCustodyStorageCompatibility(context.Context) error
	RegisterInputCustody(ctx context.Context, turnID, owner, actionKey string, at time.Time) (domain.InputCustodyReceipt, error)
	HoldConversationInput(ctx context.Context, expected domain.InputCustodyVersion, hold domain.InputHoldVersion, reason, actionKey string, at time.Time) (domain.InputCustodyReceipt, error)
	ReleaseConversationInput(ctx context.Context, expected domain.InputCustodyVersion, hold domain.InputHoldVersion, state domain.TurnState, actionKey string, at time.Time) (domain.InputCustodyReceipt, error)
	TransferInputCustody(ctx context.Context, expected domain.InputCustodyVersion, owner, actionKey string, at time.Time) (domain.InputCustodyReceipt, error)
	DeleteConversationInput(ctx context.Context, expected domain.InputCustodyVersion, actionKey string, at time.Time) (domain.InputCustodyReceipt, error)
	ConversationInputCustody(ctx context.Context, conversationID string) ([]domain.InputCustody, error)
}
