package agentaccounts

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SetProjectAccount changes only the requested local account reference.
func (s *Service) SetProjectAccount(ctx context.Context, id, accountID, role, reviewer string) error {
	if ports.IsRemoteAccountRequest(ctx) {
		return ports.ErrAgentAccountLocalOnly
	}
	account, err := s.find(ctx, accountID)
	if err != nil {
		return err
	}
	if err := accountAvailability(account); err != nil {
		return err
	}
	store, ok := s.store.(interface {
		SetProjectAgentAccount(context.Context, string, string, domain.AgentAccountProvider, string, string, domain.AgentHarness) (bool, error)
	})
	if !ok {
		return ports.ErrAgentAccountStorageUnavailable
	}
	changed, err := store.SetProjectAgentAccount(ctx, id, accountID, account.Provider, role, reviewer, s.defaultHarness)
	if err != nil {
		if errors.Is(err, ports.ErrAgentAccountProviderMismatch) || errors.Is(err, ports.ErrAgentAccountScopeInvalid) {
			return err
		}
		return ports.ErrAgentAccountStorageUnavailable
	}
	if !changed {
		return ports.ErrAgentAccountProjectUnknown
	}
	return nil
}

// SetDefaultHarness aligns inherited project roles with daemon launch policy.
// Wiring calls this before exposing the service.
func (s *Service) SetDefaultHarness(h domain.AgentHarness) {
	if h != "" {
		s.defaultHarness = h
	}
}
