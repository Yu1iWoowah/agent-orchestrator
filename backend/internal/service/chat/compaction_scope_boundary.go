package chat

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
)

func (m *sideManager) markEndedCompaction(ctx context.Context, row domain.SideCompaction) (bool, string, error) {
	ended, reason := processscope.Ended(ctx, row.Scope)
	if !ended {
		return false, reason, nil
	}
	store := m.store.(*memorySideStore)
	if err := store.durable.MarkSideCompactionHistorical(ctx, row.ID, row.Epoch, reason); err != nil {
		return false, "historical_write_failed", err
	}
	return true, reason, nil
}
