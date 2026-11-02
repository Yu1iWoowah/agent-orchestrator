package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrChatDeliveryUnverified retains the original execution instead of replaying it.
var ErrChatDeliveryUnverified = errors.New("chat delivery evidence is unavailable")

// ErrChatShutdownNotSettled refuses a normal detach without settlement evidence.
var ErrChatShutdownNotSettled = errors.New("shutdown_not_settled")

// ChatDeliveryLookup contains correlation keys only, never a prompt or path.
type ChatDeliveryLookup struct {
	AttemptID         string
	PreparedTurnID    string
	ProviderRequestID string
	ProviderEpoch     string
}

// ChatDeliveryObserver queries the original execution and flushes its durable
// custody. Missing records or an active-prompt boolean are not negative proof.
// Flush may cancel an unforwarded request only with positive cancellation proof.
type ChatDeliveryObserver interface {
	QueryDeliveryAttempt(context.Context, ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error)
	FenceAndFlushOutbound(context.Context, []ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error)
}

// ChatDeliveryAcknowledger releases host evidence only after its durable receipt
// is committed. Older observers may keep bounded unacknowledged evidence.
type ChatDeliveryAcknowledger interface {
	AcknowledgeDeliveryAttempt(context.Context, ChatDeliveryLookup) error
}

// ChatOutboundReconciler obtains fresh native state for an uncovered operation
// from its original host. Nil requires positive idle/terminal evidence and a
// successful host flush, never an empty local in-flight flag or an RPC ACK.
// This does not change canonical delivery dispositions.
type ChatOutboundReconciler interface {
	ReconcileOutbound(context.Context) error
}
