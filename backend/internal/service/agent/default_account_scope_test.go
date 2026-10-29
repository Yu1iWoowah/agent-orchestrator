package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type unavailableDefaultAccount struct{ ports.AgentAccountExecution }

func (unavailableDefaultAccount) ResolveAccount(context.Context, ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountUnknown
}

type signedOutGlobalAccount struct {
	scopedAccountExecution
	globalProbes   atomic.Int32
	globalSignedIn bool
}

func (*signedOutGlobalAccount) ResolveAccount(_ context.Context, in ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	id, source := in.AccountID, domain.AgentAccountSourceExplicit
	if id == "" {
		id, source = "global-A", domain.AgentAccountSourceGlobal
	}
	return domain.ResolvedAgentAccount{AccountID: id, Provider: domain.AgentAccountProviderCodex, Source: source}, nil
}

func (a *signedOutGlobalAccount) AccountIdentity(ctx context.Context, harness domain.AgentHarness, id string) (domain.AgentAccountIdentityCheck, error) {
	if id == "global-A" {
		a.globalProbes.Add(1)
		if !a.globalSignedIn {
			return domain.AgentAccountIdentityCheck{}, ports.ErrAgentAccountSignedOut
		}
	}
	if id == "device-codex" {
		return domain.AgentAccountIdentityCheck{}, fmt.Errorf("device must use native readiness")
	}
	return a.scopedAccountExecution.AccountIdentity(ctx, harness, id)
}

func TestSelectedAccountReadinessDoesNotReuseGlobalAuthentication(t *testing.T) {
	for _, tc := range []struct {
		deviceState    ports.AgentAuthStatus
		globalSignedIn bool
	}{
		{ports.AgentAuthStatusAuthorized, false},
		{ports.AgentAuthStatusUnauthorized, false},
		{ports.AgentAuthStatusUnauthorized, true},
	} {
		t.Run(fmt.Sprintf("device=%s/global=%t", tc.deviceState, tc.globalSignedIn), func(t *testing.T) {
			adapter := &readinessTestAgent{
				resolve: func(context.Context) (string, error) { return "/fake/codex", nil },
				auth:    func(context.Context) (ports.AgentAuthStatus, error) { return tc.deviceState, nil },
			}
			svc := newService([]agentregistry.HarnessAgent{readinessHarness("codex", "Codex", adapter)}, nil, nil)
			// Match production wiring; the bare test constructor has no saved
			// default callback and would conceal the original regression.
			svc.readiness.authenticationCheck = svc.authenticationWithAccount
			accounts := &signedOutGlobalAccount{globalSignedIn: tc.globalSignedIn}
			svc.SetAccountExecution(accounts, nil)
			ctx := context.Background()
			globalWant := domain.AgentAuthenticationUnauthorized
			if tc.globalSignedIn {
				globalWant = domain.AgentAuthenticationAuthorized
			}
			global, err := svc.EnsureAgentReadiness(ctx, "codex", domain.AgentReadinessPurposeLaunch)
			if err != nil || global.Authentication.State != globalWant {
				t.Fatalf("global readiness: %+v %v", global, err)
			}
			for _, id := range []string{"device-codex", "managed-B"} {
				selected := domain.ResolvedAgentAccount{AccountID: id, Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceExplicit}
				selectedCtx := ports.WithAgentAccount(ctx, selected)
				want := domain.AgentAuthenticationAuthorized
				if id == "device-codex" && tc.deviceState == ports.AgentAuthStatusUnauthorized {
					want = domain.AgentAuthenticationUnauthorized
				}
				for range 2 {
					got, err := svc.EnsureAgentReadiness(selectedCtx, "codex", domain.AgentReadinessPurposeLaunch)
					if err != nil || got.Authentication.State != want || got.AccountBinding == nil || *got.AccountBinding != selected {
						t.Fatalf("selected %s: %+v %v", id, got, err)
					}
				}
			}
			if accounts.globalProbes.Load() != 1 || adapter.authCalls.Load() != 1 {
				t.Fatalf("cache crossed accounts or device lost TTL: global=%d device=%d", accounts.globalProbes.Load(), adapter.authCalls.Load())
			}
			global, err = svc.EnsureAgentReadiness(ctx, "codex", domain.AgentReadinessPurposeLaunch)
			if err != nil || global.Authentication.State != globalWant {
				t.Fatalf("device overwrote global readiness: %+v %v", global, err)
			}
			svc.InvalidateAgentAuthentication("codex")
			deviceCtx := ports.WithAgentAccount(ctx, domain.ResolvedAgentAccount{AccountID: "device-codex", Provider: domain.AgentAccountProviderCodex})
			if _, err := svc.EnsureAgentReadiness(deviceCtx, "codex", domain.AgentReadinessPurposeLaunch); err != nil || adapter.authCalls.Load() != 2 {
				t.Fatalf("device cache ignored invalidation: calls=%d err=%v", adapter.authCalls.Load(), err)
			}
			before := adapter.resolveCalls.Load()
			svc.InvalidateAgentInstallation("codex")
			if _, err := svc.EnsureAgentReadiness(deviceCtx, "codex", domain.AgentReadinessPurposeLaunch); err != nil || adapter.resolveCalls.Load() <= before {
				t.Fatalf("device installation cache ignored invalidation: calls=%d err=%v", adapter.resolveCalls.Load(), err)
			}
		})
	}
}

func TestAuthenticationCallbackUsesSelectedAccountBeforeGlobal(t *testing.T) {
	for _, id := range []string{"device-codex", "managed-B", ""} {
		t.Run(id, func(t *testing.T) {
			svc := newService(nil, nil, nil)
			accounts := &signedOutGlobalAccount{}
			svc.SetAccountExecution(accounts, nil)
			ctx := context.Background()
			if id != "" {
				ctx = ports.WithAgentAccount(ctx, domain.ResolvedAgentAccount{AccountID: id, Provider: domain.AgentAccountProviderCodex})
			}
			got, handled := svc.authenticationWithAccount(ctx, "codex", domain.AgentReadinessPurposeLaunch)
			switch id {
			case "device-codex":
				if handled || accounts.globalProbes.Load() != 0 {
					t.Fatalf("device callback queried global: %+v handled=%t", got, handled)
				}
			case "managed-B":
				if !handled || got.State != domain.AgentAuthenticationAuthorized || got.Account == nil || got.Account.Email != id+"@example.test" || accounts.globalProbes.Load() != 0 {
					t.Fatalf("managed callback queried global: %+v handled=%t", got, handled)
				}
			default:
				if !handled || got.State != domain.AgentAuthenticationUnauthorized || accounts.globalProbes.Load() != 1 {
					t.Fatalf("inherited callback skipped global: %+v handled=%t", got, handled)
				}
			}
		})
	}
}
func TestConfiguredUnavailableDefaultDoesNotFallBackToDeviceReadiness(t *testing.T) {
	svc := newService(nil, nil, nil)
	svc.SetAccountExecution(unavailableDefaultAccount{}, nil)
	observation, handled := svc.savedDefaultAuthentication(context.Background(), "codex")
	if !handled || observation.State != domain.AgentAuthenticationUnknown || observation.Account != nil {
		t.Fatalf("unavailable managed default fell back to device: %+v handled=%v", observation, handled)
	}
}
