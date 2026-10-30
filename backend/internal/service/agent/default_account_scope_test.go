package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
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

func TestDeviceDefaultSharesWarmReadiness(t *testing.T) {
	adapter := &readinessTestAgent{resolve: func(context.Context) (string, error) { return "/fake/codex", nil }, auth: func(context.Context) (ports.AgentAuthStatus, error) { return ports.AgentAuthStatusAuthorized, nil }}
	svc := newService([]agentregistry.HarnessAgent{readinessHarness("codex", "Codex", adapter)}, nil, nil)
	svc.SetAccountExecution(deviceAccountExecution{t: t}, nil)
	svc.readiness.authenticationCheck = svc.authenticationWithAccount
	<-svc.WarmReadiness()
	ctx := ports.WithAgentAccount(context.Background(), domain.ResolvedAgentAccount{AccountID: "device-codex", Provider: domain.AgentAccountProviderCodex})
	got, err := svc.EnsureAgentReadiness(ctx, "codex", domain.AgentReadinessPurposeLaunch)
	if err != nil || got.Authentication.State != domain.AgentAuthenticationAuthorized || adapter.authCalls.Load() != 1 {
		t.Fatalf("warm observation was not reused: calls=%d %+v %v", adapter.authCalls.Load(), got, err)
	}
}

func TestLegacyIdentityConflictLoadsSavedMetadataWithoutChangingCredentials(t *testing.T) {
	for _, signedOut := range []bool{false, true} {
		t.Run(fmt.Sprint("signedOut=", signedOut), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "legacy")
			pending := filepath.Join(t.TempDir(), "pending")
			catalog := newCodexAccountCatalog(root, nil)
			email := "fake-legacy@example.invalid"
			record := commitTestAccount(t, catalog, pending, "b60a377d-da68-4a61-86f2-f31f04c571f2", ports.CodexAccountObservation{Email: &email, Method: domain.CodexAuthMethodChatGPT, Authentication: domain.AgentAuthenticationAuthorized})
			credential := filepath.Join(record.Home, codexCredentialFilename)
			if signedOut {
				if err := os.Remove(credential); err != nil {
					t.Fatal(err)
				}
			}
			before, readErr := os.ReadFile(credential)
			beforeInfo, statErr := os.Lstat(credential)
			svc := newService(nil, nil, nil)
			svc.codexAccounts = newCodexAccountManager(context.Background(), root, pending, "", "", nil, nil)
			for range 2 {
				conflict, err := svc.LegacyIdentityConflict(context.Background(), domain.AgentAccountProviderCodex, strings.ToUpper(email))
				if err != nil || !conflict {
					t.Fatalf("saved legacy identity was missed: %t %v", conflict, err)
				}
			}
			after, afterReadErr := os.ReadFile(credential)
			afterInfo, afterStatErr := os.Lstat(credential)
			if signedOut {
				if !errors.Is(afterReadErr, os.ErrNotExist) || !errors.Is(afterStatErr, os.ErrNotExist) {
					t.Fatal("signed-out legacy credential recreated")
				}
			} else if readErr != nil || statErr != nil || afterReadErr != nil || afterStatErr != nil || !bytes.Equal(before, after) || beforeInfo.Mode() != afterInfo.Mode() {
				t.Fatal("legacy credential content/mode changed")
			}
		})
	}
}

func TestDeviceReadinessUnknownHarnessKeepsAPIErrorContract(t *testing.T) {
	svc := newService(nil, nil, nil)
	ctx := ports.WithAgentAccount(context.Background(), domain.ResolvedAgentAccount{AccountID: "device-codex", Provider: domain.AgentAccountProviderCodex})
	_, err := svc.EnsureAgentReadiness(ctx, "unknown-harness", domain.AgentReadinessPurposeLaunch)
	var api *apierr.Error
	if !errors.As(err, &api) || api.Code != "UNKNOWN_AGENT_ID" {
		t.Fatalf("device path changed unknown harness error: %v", err)
	}
}
