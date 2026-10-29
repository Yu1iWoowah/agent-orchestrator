package agentaccounts

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SetUsageOwners adds in-process launch facts to the durable owner projection.
// It is wired once before API requests or lifecycle recovery begin.
func (s *Service) SetUsageOwners(owners func(context.Context, string) ([]domain.AgentAccountUsageOwner, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usageOwners = owners
}

// UsageOwners includes durable users and in-flight launches for a local account.
func (s *Service) UsageOwners(ctx context.Context, id string) ([]domain.AgentAccountUsageOwner, error) {
	if _, err := s.find(ctx, id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	lookup := s.usageOwners
	s.mu.Unlock()
	if lookup != nil {
		return lookup(ctx, id)
	}
	if store, ok := s.store.(ports.AgentAccountUsageStore); ok {
		owners, err := store.ListAgentAccountUsageOwners(ctx, id)
		if err != nil {
			return nil, ports.ErrAgentAccountStorageUnavailable
		}
		return owners, nil
	}
	return []domain.AgentAccountUsageOwner{}, nil
}

func (s *Service) refuseLiveOwners(ctx context.Context, id string) error {
	owners, err := s.UsageOwners(ctx, id)
	if err != nil {
		return ports.ErrAgentAccountStorageUnavailable
	}
	for _, owner := range owners {
		if owner.Blocking {
			return &ports.AgentAccountInUseError{Owners: owners}
		}
	}
	return nil
}
