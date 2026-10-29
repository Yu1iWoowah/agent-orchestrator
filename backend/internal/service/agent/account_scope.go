package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SetAccountExecution is wired once before request handling begins.
func (s *Service) SetAccountExecution(accounts ports.AgentAccountExecution, projects ports.AgentAccountProjects) {
	s.accountExecution = accounts
	s.accountProjects = projects
}

func accountCatalogScope(ctx context.Context, agentID, scope string) (string, error) {
	if selected, ok := ports.AgentAccountFromContext(ctx); ok {
		p, supported := domain.AgentAccountProviderFor(domain.AgentHarness(agentID))
		if !supported || selected.Provider != p {
			return "", ports.ErrAgentAccountProviderMismatch
		}
		if id, encoded := ports.ParseAccountModelCatalogScope(scope); encoded && id != selected.AccountID {
			return "", ports.ErrAgentAccountChanged
		}
		return ports.AccountModelCatalogScope(selected.AccountID), nil
	}
	return scope, nil
}

// EnsureAgentAccountReadiness never asks a device/default auth adapter for the
// sign-in of a managed execution. Installation observations are shared; account
// observations remain on this response and never overwrite another identity.
func (s *Service) EnsureAgentAccountReadiness(ctx context.Context, agentID string, purpose domain.AgentReadinessPurpose, id string) (domain.AgentReadinessSnapshot, error) {
	if s.accountExecution == nil {
		return domain.AgentReadinessSnapshot{}, ports.ErrAgentAccountStorageUnavailable
	}
	items, err := s.readiness.EnsureInstallation(ctx, []string{agentID}, purpose)
	if err != nil {
		return domain.AgentReadinessSnapshot{}, err
	}
	if len(items) == 0 || items[0].Installation.State != domain.AgentInstallationInstalled {
		return domain.AgentReadinessSnapshot{}, ports.ErrAgentBinaryNotFound
	}
	identity, err := s.accountExecution.AccountIdentity(ctx, domain.AgentHarness(agentID), id)
	if err != nil {
		return domain.AgentReadinessSnapshot{}, err
	}
	got := items[0]
	now := s.now()
	state := domain.AgentAuthenticationUnauthorized
	if identity.SignedIn {
		state = domain.AgentAuthenticationAuthorized
	} else if identity.LoggedIn {
		state = domain.AgentAuthenticationConfigured
	}
	got.Authentication = domain.AgentAuthenticationObservation{State: state, Freshness: domain.AgentReadinessFresh, CheckedAt: &now, AttemptedAt: &now, ReasonCode: "selected_account"}
	if identity.LoggedIn {
		method := domain.AgentAccountMethodAccount
		if !identity.SignedIn {
			method = domain.AgentAccountMethodAPIKey
		}
		got.Authentication.Account = domain.AgentAccountIdentity{Email: identity.Email, Plan: identity.Plan, Method: method}.Sanitized()
	}
	if selected, ok := ports.AgentAccountFromContext(ctx); ok {
		got.AccountBinding = &selected
	} else {
		p, _ := domain.AgentAccountProviderFor(domain.AgentHarness(agentID))
		got.AccountBinding = &domain.ResolvedAgentAccount{AccountID: id, Provider: p, Source: domain.AgentAccountSourceExplicit}
	}
	got.EffectiveReadiness = domain.EffectiveAgentReadiness(got.Installation.State, state)
	return got, nil
}

func withAccountCatalogBinding(ctx context.Context, catalog ports.AgentModelCatalog) ports.AgentModelCatalog {
	if selected, ok := ports.AgentAccountFromContext(ctx); ok {
		catalog.AccountBinding = &selected
	}
	return catalog
}

// AccountContext resolves API preflight with the same daemon snapshot/priority
// as spawn. Encoded cache keys are internal and cannot be supplied as projects.
func (s *Service) AccountContext(ctx context.Context, harness domain.AgentHarness, projectID string, role domain.AgentAccountRole, id string) (context.Context, error) {
	if ports.IsRemoteAccountRequest(ctx) && id != "" {
		return ctx, ports.ErrAgentAccountLocalOnly
	}
	if s.accountExecution == nil {
		if id != "" {
			return ctx, ports.ErrAgentAccountStorageUnavailable
		}
		return ctx, nil
	}
	if _, credential := credentialTypeFromScope(projectID); credential {
		if id != "" {
			return ctx, ports.ErrAgentAccountLocalOnly
		}
		return ctx, nil
	}
	if strings.HasPrefix(projectID, "@account:") {
		return ctx, apierr.Invalid("INVALID_MODEL_SCOPE", "Account cache scopes are internal.", nil)
	}
	if role == "" {
		role = domain.AgentAccountRoleWorker
	}
	if role != domain.AgentAccountRoleWorker && role != domain.AgentAccountRoleOrchestrator && role != domain.AgentAccountRoleReviewer {
		return ctx, apierr.Invalid("INVALID_AGENT_ACCOUNT_ROLE", "Unknown account role.", nil)
	}
	var config domain.ProjectConfig
	if projectID != "" {
		if s.accountProjects == nil {
			return ctx, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
		}
		project, found, err := s.accountProjects.GetProject(ctx, projectID)
		if err != nil {
			return ctx, err
		}
		if !found {
			return ctx, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
		}
		config = project.Config
	}
	selected, err := s.accountExecution.ResolveAccount(ctx, ports.AgentAccountResolutionRequest{Harness: harness, Role: role, ProjectConfig: config, AccountID: id})
	if err != nil {
		return ctx, err
	}
	return ports.WithAgentAccount(ctx, selected), nil
}

// configuredDefaultAuthentication never probes the device after an invalid or
// uncertain managed default. Unknown remains unknown, rather than signed out.
func (s *Service) configuredDefaultAuthentication(ctx context.Context, harness domain.AgentHarness) (domain.AgentAuthenticationObservation, bool) {
	provider, supported := domain.AgentAccountProviderFor(harness)
	if !supported {
		return domain.AgentAuthenticationObservation{}, false
	}
	selected, scoped := ports.AgentAccountFromContext(ctx)
	var err error
	if scoped {
		if selected.Provider != provider {
			err = ports.ErrAgentAccountProviderMismatch
		}
	} else {
		selected, err = s.accountExecution.ResolveAccount(ctx, ports.AgentAccountResolutionRequest{Harness: harness})
	}
	if err == nil && selected.AccountID == domain.DeviceAgentAccountID(provider) {
		return domain.AgentAuthenticationObservation{}, false
	}
	var identity domain.AgentAccountIdentityCheck
	if err == nil {
		identity, err = s.accountExecution.AccountIdentity(ctx, harness, selected.AccountID)
	}
	now := time.Now().UTC()
	var observation domain.AgentAuthenticationObservation
	switch {
	case errors.Is(err, ports.ErrAgentAccountSignedOut):
		observation = successfulAuthentication(now, domain.AgentAuthenticationUnauthorized, domain.AgentReadinessReasonUnauthorized, "The configured account needs sign-in. Sign in again in Settings.")
	case err != nil:
		observation = failedAuthentication(now, domain.AgentReadinessReasonAuthCheckInconclusive, "The configured account is unavailable or could not be checked. Retry in Settings.")
	case identity.SignedIn:
		observation = successfulAuthentication(now, domain.AgentAuthenticationAuthorized, domain.AgentReadinessReasonAuthorized, "The configured account is signed in.")
		observation.Account = domain.AgentAccountIdentity{Email: identity.Email, Plan: identity.Plan, Method: domain.AgentAccountMethodAccount}.Sanitized()
	default:
		observation = successfulAuthentication(now, domain.AgentAuthenticationUnauthorized, domain.AgentReadinessReasonUnauthorized, "The configured account needs sign-in. Sign in again in Settings.")
	}
	observation.Diagnostics = []domain.AgentAuthDiagnostic{domain.NewAgentAuthDiagnostic("agent_account", "saved_default_account")}
	return observation, true
}
