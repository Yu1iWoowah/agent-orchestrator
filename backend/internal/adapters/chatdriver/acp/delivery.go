package acp

import (
	"context"
	"encoding/json"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const promptOperation = "ACP session/prompt"

// IsArchivedPromptStall reads the original JSON-RPC error already saved on the
// turn. Reuse the live marker so an ordinary provider error cannot restore a hold.
func (*conversation) IsArchivedPromptStall(message string) bool {
	payload, ok := strings.CutPrefix(message, promptOperation+": ")
	var native acpsdk.RequestError
	return ok && json.Unmarshal([]byte(payload), &native) == nil && isACPPromptStalled(&native)
}

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
