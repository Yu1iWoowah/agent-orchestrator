package agentaccounts

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ResolveAccount chooses metadata once, without starting a provider process.
// Native identity/file evidence is validated by the exact-account preflight.
// Only omission inherits. A selected but unusable account is always an error.
func (s *Service) ResolveAccount(ctx context.Context, in ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	if ports.IsRemoteAccountRequest(ctx) && in.AccountID != "" {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountLocalOnly
	}
	id, source := in.AccountID, domain.AgentAccountSourceExplicit
	if id == "" {
		id, source = in.ProjectConfig.RoleAccountReference(in.Role, in.Harness), domain.AgentAccountSourceRole
	}
	provider, supported := domain.AgentAccountProviderFor(in.Harness)
	if !supported || s.providers[provider] == nil {
		if id != "" {
			return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountProviderMismatch
		}
		return domain.ResolvedAgentAccount{}, nil
	}
	if id == "" {
		id, source = in.ProjectConfig.AgentAccounts[provider], domain.AgentAccountSourceProject
	}
	if id == "" {
		var set bool
		var err error
		id, set, err = s.store.GetAgentAccountDefault(ctx, provider)
		if err != nil {
			return domain.ResolvedAgentAccount{}, err
		}
		source = domain.AgentAccountSourceGlobal
		if !set {
			id, source = domain.DeviceAgentAccountID(provider), domain.AgentAccountSourceDevice
		}
		if id == "" {
			return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountUnknown
		}
	}
	if ports.IsRemoteAccountRequest(ctx) && id != domain.DeviceAgentAccountID(provider) {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountLocalOnly
	}
	account, err := s.find(ctx, id)
	if err != nil {
		return domain.ResolvedAgentAccount{}, err
	}
	if account.Provider != provider {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountProviderMismatch
	}
	if err := accountAvailability(account); err != nil {
		return domain.ResolvedAgentAccount{}, err
	}
	if r := s.cachedReading(account.ID); !account.Device() && r != nil && r.knownUnusable() && s.now().Sub(r.at) <= freshFor {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountSignedOut
	}
	return domain.ResolvedAgentAccount{AccountID: account.ID, Provider: provider, Source: source}, nil
}

// AccountExecutionEnv revalidates the selected account; it never resolves defaults.
func (s *Service) AccountExecutionEnv(ctx context.Context, harness domain.AgentHarness, id string) (map[string]string, error) {
	account, err := s.pick(ctx, harness, id)
	if err != nil {
		return nil, err
	}
	return s.providers[account.Provider].LaunchEnv(account.Home), nil
}

// AccountIdentity checks only the selected native profile.
func (s *Service) AccountIdentity(ctx context.Context, harness domain.AgentHarness, id string) (domain.AgentAccountIdentityCheck, error) {
	account, err := s.pick(ctx, harness, id)
	if err != nil {
		return domain.AgentAccountIdentityCheck{}, err
	}
	r := s.cachedReading(account.ID)
	if r == nil {
		return domain.AgentAccountIdentityCheck{}, ports.ErrAgentAccountStorageUnavailable
	}
	return r.identity, nil
}

// SessionAccountBinding restores a persisted selection without resolving any
// current default. Missing facts remain an error rather than becoming device.
func (s *Service) SessionAccountBinding(ctx context.Context, id domain.SessionID, harness domain.AgentHarness) (domain.ResolvedAgentAccount, error) {
	provider, supported := domain.AgentAccountProviderFor(harness)
	if !supported {
		return domain.ResolvedAgentAccount{}, nil
	}
	binding, found, err := s.store.GetSessionAgentAccountBinding(ctx, id, provider)
	if err != nil {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountStorageUnavailable
	}
	if !found || binding.AccountID == "" {
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountUnknown
	}
	return binding, nil
}

// BindResolvedSession records the selected metadata before any native check.
// A failed preflight is handled by the manager's existing seed rollback.
func (s *Service) BindResolvedSession(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, selected domain.ResolvedAgentAccount) error {
	p, supported := domain.AgentAccountProviderFor(harness)
	if !supported {
		if selected.AccountID != "" {
			return ports.ErrAgentAccountProviderMismatch
		}
		return nil
	}
	if selected.Provider != p || selected.AccountID == "" {
		return ports.ErrAgentAccountProviderMismatch
	}
	account, err := s.find(ctx, selected.AccountID)
	if err != nil {
		return err
	}
	if account.Provider != p {
		return ports.ErrAgentAccountProviderMismatch
	}
	if err := accountAvailability(account); err != nil {
		return err
	}
	if err := s.store.BindSessionAgentAccountWithSource(ctx, id, selected, s.now()); err != nil {
		return err
	}
	if account.Device() {
		return nil
	}
	_, err = s.pick(ctx, harness, selected.AccountID)
	return err
}

// DescribeAccountBinding never probes a provider or exposes a profile path.
func (s *Service) DescribeAccountBinding(ctx context.Context, b domain.ResolvedAgentAccount) (domain.AgentAccountBindingView, error) {
	view := domain.AgentAccountBindingView{AccountID: b.AccountID, Provider: b.Provider, Source: b.Source}
	account, err := s.find(ctx, b.AccountID)
	if err != nil {
		if errors.Is(err, ports.ErrAgentAccountUnknown) {
			return view, nil
		}
		return view, ports.ErrAgentAccountStorageUnavailable
	}
	view.Label = account.Label
	if view.Label == "" {
		view.Label = account.Email
	}
	if account.Device() {
		view.Label = "This computer"
	}
	if accountAvailability(account) == nil {
		r := s.cachedReading(account.ID)
		view.Available = account.Device() || r != nil && r.identityErr == "" && !r.knownUnusable()
	}
	return view, nil
}

// SessionAccountBindings projects durable choices without native probes.
func (s *Service) SessionAccountBindings(ctx context.Context, id domain.SessionID) (map[domain.AgentAccountProvider]domain.AgentAccountBindingView, error) {
	views := map[domain.AgentAccountProvider]domain.AgentAccountBindingView{}
	for _, p := range domain.AgentAccountProviders {
		binding, found, err := s.store.GetSessionAgentAccountBinding(ctx, id, p)
		if err != nil {
			return nil, ports.ErrAgentAccountStorageUnavailable
		}
		if !found {
			continue
		}
		view, err := s.DescribeAccountBinding(ctx, binding)
		if err != nil {
			return nil, err
		}
		views[p] = view
	}
	return views, nil
}
