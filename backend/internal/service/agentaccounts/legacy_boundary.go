package agentaccounts

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SetLegacyIdentityCheck uses saved legacy metadata only. It neither migrates
// credentials nor equates legacy catalog IDs with managed account IDs.
func (s *Service) SetLegacyIdentityCheck(check func(context.Context, domain.AgentAccountProvider, string) (bool, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legacyIdentityCheck = check
}

// ValidateAccountHandoff refreshes the target and rejects implicit external history imports.
func (s *Service) ValidateAccountHandoff(ctx context.Context, harness domain.AgentHarness, sourceID, targetID, nativeID string) error {
	target, err := s.find(ctx, targetID)
	if err != nil {
		return err
	}
	if !target.Device() {
		s.read(ctx, target)
	} // refresh even if the ordinary catalog TTL is fresh
	if _, err = s.pick(ctx, harness, targetID); err != nil {
		return err
	}
	if nativeID == "" || target.ProfileKind != domain.AgentAccountProfileContained {
		return nil
	}
	source, err := s.find(ctx, sourceID)
	if err != nil {
		return err
	}
	if source.Device() || source.ProfileKind != domain.AgentAccountProfileContained {
		return ports.ErrAgentAccountUnshared
	}
	return nil
}
