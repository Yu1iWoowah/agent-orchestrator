package ports

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// OperatorConsentStore is for admitted domain services, not an HTTP mint API.
// Production has no ConfirmFromNative implementation until the native issuer
// and both owned/attach authority chains are delivered in C0-PR2.
type OperatorConsentStore interface {
	PrepareOperatorAction(context.Context, domain.OperatorActionPlan) (domain.OperatorActionReceipt, error)
	GetOperatorAction(context.Context, string) (domain.OperatorActionReceipt, error)
	RevokeUnused(context.Context, domain.OperatorRequestRef) error
}

// OperatorConsentTransaction participates in the domain owner's existing SQLite
// transaction. It cannot commit, open another transaction, acquire a domain lock
// or perform provider I/O. The adapter supplies it only within that transaction;
// any consume or reservation error must roll back both writes. A Replay receipt
// refers to the original reservation and must never dispatch another execution.
type OperatorConsentTransaction interface {
	ConsumeInActionTransaction(context.Context, domain.OperatorConsentRef, domain.OperatorActionPlan, string, string) (domain.OperatorConsumption, error)
}
