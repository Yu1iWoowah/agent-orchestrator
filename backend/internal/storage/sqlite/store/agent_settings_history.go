package store

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ListAgentSettingsOperations is a bounded journal read, with no provider work.
func (s *Store) ListAgentSettingsOperations(ctx context.Context, id domain.SessionID, maximum int) ([]domain.AgentSettingsOperation, error) {
	if maximum < 1 || maximum > 100 {
		return nil, ports.ErrAgentSettingsInvalid
	}
	rows, err := s.qr.ListAgentSettingsOperations(ctx, gen.ListAgentSettingsOperationsParams{SessionID: string(id), Maximum: int64(maximum)})
	if err != nil {
		return nil, err
	}
	result := make([]domain.AgentSettingsOperation, 0, len(rows))
	for _, row := range rows {
		op, err := decodeSettingsOperation(row)
		if err != nil {
			return nil, err
		}
		result = append(result, op)
	}
	return result, nil
}
