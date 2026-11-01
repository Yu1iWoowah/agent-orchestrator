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
