package codexappserver

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (c *conversation) QueryDeliveryAttempt(ctx context.Context, key ports.ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error) {
	if c.proc.delivery == nil {
		return domain.ChatDeliveryReceipt{}, ports.ErrChatDeliveryUnverified
	}
	return c.proc.delivery.QueryDeliveryAttempt(ctx, key)
}
func (c *conversation) FenceAndFlushOutbound(ctx context.Context, keys []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	if c.proc.delivery == nil {
		return nil, ports.ErrChatDeliveryUnverified
	}
	return c.proc.delivery.FenceAndFlushOutbound(ctx, keys)
}
