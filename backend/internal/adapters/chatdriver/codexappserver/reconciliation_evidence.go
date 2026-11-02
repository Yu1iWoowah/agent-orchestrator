package codexappserver

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ReconciliationEvidence attests only the adapter's per-request settings wire.
// It deliberately supplies no sticky current-value or provider-epoch claim.
func (c *conversation) ReconciliationEvidence(ctx context.Context, owner domain.AgentSettingsOwner) (ports.AgentSettingsReconciliationEvidence, error) {
	if err := ctx.Err(); err != nil {
		return ports.AgentSettingsReconciliationEvidence{}, err
	}
	if owner.ProviderConversationID != c.ProviderConversationID() {
		return ports.AgentSettingsReconciliationEvidence{}, ports.ErrAgentSettingsConflict
	}
	return ports.AgentSettingsReconciliationEvidence{Kind: "per_turn_wire", Owner: owner, SourceRef: "codex.turn.start.settings", PermissionVerified: true}, nil
}
