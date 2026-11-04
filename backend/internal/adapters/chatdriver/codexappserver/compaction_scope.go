package codexappserver

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (c *conversation) CompactionScope(ctx context.Context) (ports.ChatCompactionScope, error) {
	if c.proc == nil || c.proc.delivery == nil {
		return ports.ChatCompactionScope{}, nil
	}
	return c.proc.delivery.CompactionScope(ctx)
}
