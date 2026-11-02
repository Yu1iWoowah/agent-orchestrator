package codexappserver

import (
	"context"
	"encoding/json"

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

func (c *conversation) AcknowledgeDeliveryAttempt(ctx context.Context, key ports.ChatDeliveryLookup) error {
	if c.proc.delivery == nil {
		return ports.ErrChatDeliveryUnverified
	}
	return c.proc.delivery.AcknowledgeDeliveryAttempt(ctx, key)
}

// ReconcileOutbound uses an includeTurns read through the original host. The
// host correlates that native response with already-responded coverage gaps;
// a cached status, another thread, or a failed read cannot release them.
func (c *conversation) ReconcileOutbound(ctx context.Context) error {
	if c.proc.delivery == nil {
		return ports.ErrChatDeliveryUnverified
	}
	var response struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
			Turns json.RawMessage `json:"turns"`
		} `json:"thread"`
	}
	if err := c.conn.request(ctx, "thread/read", map[string]any{"threadId": c.threadID, "includeTurns": true}, &response); err != nil {
		return ports.ErrChatDeliveryUnverified
	}
	if response.Thread.ID != c.threadID || response.Thread.Status.Type != "idle" || len(response.Thread.Turns) == 0 || string(response.Thread.Turns) == "null" {
		return ports.ErrChatShutdownNotSettled
	}
	_, err := c.proc.delivery.FenceAndFlushOutbound(ctx, nil)
	return err
}
