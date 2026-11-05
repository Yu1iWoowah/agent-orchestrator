package chat

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
)

func markEndedCompaction(ctx context.Context, store ports.SideCompactionStore, row domain.SideCompaction) (bool, string, error) {
	ended, reason := processscope.Ended(ctx, row.Scope)
	if !ended {
		return false, reason, nil
	}
	if err := store.MarkSideCompactionHistorical(ctx, row.ID, row.Epoch, reason); err != nil {
		return false, "historical_write_failed", err
	}
	return true, reason, nil
}
