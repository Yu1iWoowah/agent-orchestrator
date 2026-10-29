package agentaccounts

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ResolveReviewerAccount resolves only new executions. An existing row is the
// authority for all passes/restores, including legacy device-compatible rows.
func (s *Service) ResolveReviewerAccount(ctx context.Context, id string, in ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	if ports.IsRemoteAccountRequest(ctx) && in.AccountID != "" {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountLocalOnly
	}
	p, supported := domain.AgentAccountProviderFor(in.Harness)
	if !supported {
		return s.ResolveAccount(ctx, in)
	}
	store, ok := s.store.(ports.ReviewerAgentAccountStore)
	if !ok || id == "" {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountStorageUnavailable
	}
	selected, found, err := store.GetReviewerAgentAccount(ctx, id, p)
	if err != nil {
		return domain.ResolvedAgentAccount{}, err
	}
	if !found {
		selected, err = s.ResolveAccount(ctx, in)
		if err != nil {
			return domain.ResolvedAgentAccount{}, err
		}
		if err := store.BindReviewerAgentAccount(ctx, id, selected, s.now()); err != nil {
			return domain.ResolvedAgentAccount{}, err
		}
		selected, found, err = store.GetReviewerAgentAccount(ctx, id, p)
		if err != nil {
			return domain.ResolvedAgentAccount{}, err
		}
		if !found {
			return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountStorageUnavailable
		}
	}
	if in.AccountID != "" && in.AccountID != selected.AccountID {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountChanged
	}
	if ports.IsRemoteAccountRequest(ctx) && selected.AccountID != domain.DeviceAgentAccountID(p) {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountLocalOnly
	}
	account, err := s.find(ctx, selected.AccountID)
	if err != nil {
		return domain.ResolvedAgentAccount{}, err
	}
	if account.Provider != p {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountProviderMismatch
	}
	if err := accountAvailability(account); err != nil {
		return domain.ResolvedAgentAccount{}, err
	}
	return selected, nil
}

// ReviewerLaunchEnv revalidates the independent reviewer binding.
func (s *Service) ReviewerLaunchEnv(ctx context.Context, id string, harness domain.AgentHarness) (map[string]string, error) {
	p, supported := domain.AgentAccountProviderFor(harness)
	if !supported {
		return nil, nil
	}
	store, ok := s.store.(ports.ReviewerAgentAccountStore)
	if !ok || id == "" {
		return nil, ports.ErrAgentAccountStorageUnavailable
	}
	if _, found, err := store.GetReviewerAgentAccount(ctx, id, p); err != nil {
		return nil, err
	} else if !found {
		return nil, ports.ErrAgentAccountUnknown
	}
	return s.launchEnv(ctx, domain.SessionID(id), domain.AgentAccount{}, harness, id)
}

func (s *Service) reviewerAccount(ctx context.Context, id string, p domain.AgentAccountProvider) (domain.AgentAccount, bool, error) {
	store, ok := s.store.(ports.ReviewerAgentAccountStore)
	if !ok {
		return domain.AgentAccount{}, false, ports.ErrAgentAccountStorageUnavailable
	}
	binding, bound, err := store.GetReviewerAgentAccount(ctx, id, p)
	if err != nil {
		return domain.AgentAccount{}, false, err
	}
	accountID := binding.AccountID
	if !bound {
		accountID, err = s.defaultID(ctx, p)
		if err != nil {
			return domain.AgentAccount{}, false, err
		}
	}
	account, err := s.find(ctx, accountID)
	if errors.Is(err, ports.ErrAgentAccountUnknown) {
		retired := time.Unix(0, 0).UTC()
		return domain.AgentAccount{ID: accountID, Provider: p, RetiredAt: &retired}, bound, nil
	}
	return account, bound, err
}

func (s *Service) bindExecutionAccount(ctx context.Context, id domain.SessionID, reviewID string, p domain.AgentAccountProvider, account domain.AgentAccount) error {
	source := domain.AgentAccountSourceGlobal
	if account.Device() {
		source = domain.AgentAccountSourceDevice
	}
	selected := domain.ResolvedAgentAccount{AccountID: account.ID, Provider: p, Source: source}
	if reviewID == "" {
		return s.store.BindSessionAgentAccountWithSource(ctx, id, selected, s.now())
	}
	store, ok := s.store.(ports.ReviewerAgentAccountStore)
	if !ok {
		return ports.ErrAgentAccountStorageUnavailable
	}
	return store.BindReviewerAgentAccount(ctx, reviewID, selected, s.now())
}
