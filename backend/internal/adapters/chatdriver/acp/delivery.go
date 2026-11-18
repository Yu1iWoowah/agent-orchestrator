package acp

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
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

// OutboundProviderEpoch names the attached persistent host; empty without one.
func (c *conversation) OutboundProviderEpoch() string {
	if c.proc.delivery == nil {
		return ""
	}
	return c.proc.delivery.ProviderEpoch()
}

// DeliveryEpochFromPreviousBoot proves an epoch's host belongs to an earlier boot.
func (*conversation) DeliveryEpochFromPreviousBoot(epoch string) bool {
	return persistenthost.EpochFromPreviousBoot(epoch)
}

func (c *conversation) AcknowledgeDeliveryAttempt(ctx context.Context, key ports.ChatDeliveryLookup) error {
	if c.proc.delivery == nil {
		return ports.ErrChatDeliveryUnverified
	}
	return c.proc.delivery.AcknowledgeDeliveryAttempt(ctx, key)
}
