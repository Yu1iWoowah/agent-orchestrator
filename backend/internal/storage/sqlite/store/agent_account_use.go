package store

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ListAgentAccountUsageOwners returns local execution metadata without profiles.
func (s *Store) ListAgentAccountUsageOwners(ctx context.Context, id string) ([]domain.AgentAccountUsageOwner, error) {
	rows, err := s.qr.ListAgentAccountUsageOwners(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]domain.AgentAccountUsageOwner, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.AgentAccountUsageOwner{Kind: row.OwnerKind, ProjectID: domain.ProjectID(row.ProjectID), SessionID: row.SessionID, SessionName: row.SessionName, ReviewID: row.ReviewID, PRURL: row.PRURL, Harness: row.Harness, TransitionID: row.TransitionID, Blocking: row.Blocking != 0})
	}
	return out, nil
}
