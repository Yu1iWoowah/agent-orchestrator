package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type scopedAccountExecution struct{}

func (scopedAccountExecution) ResolveAccount(_ context.Context, in ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	p, _ := domain.AgentAccountProviderFor(in.Harness)
	return domain.ResolvedAgentAccount{AccountID: in.AccountID, Provider: p, Source: domain.AgentAccountSourceExplicit}, nil
}
func (scopedAccountExecution) AccountExecutionEnv(_ context.Context, _ domain.AgentHarness, id string) (map[string]string, error) {
	return map[string]string{"CODEX_HOME": "/fake/" + id, "AO_CODEX_MANAGED_ACCOUNT": "1", "OPENAI_API_KEY": ""}, nil
}
func (scopedAccountExecution) AccountIdentity(_ context.Context, _ domain.AgentHarness, id string) (domain.AgentAccountIdentityCheck, error) {
	return domain.AgentAccountIdentityCheck{LoggedIn: true, SignedIn: true, Email: id + "@example.test"}, nil
}

type deviceProbeRefuser struct {
	fakeAgent
	calls atomic.Int32
}

func (a *deviceProbeRefuser) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	a.calls.Add(1)
	return ports.AgentAuthStatusUnauthorized, nil
}

func TestManagedReadinessChecksSelectedAccountWithoutDeviceAuth(t *testing.T) {
	adapter := &deviceProbeRefuser{}
	item := harnessAgent("codex", "Codex", nil)
	item.Agent = adapter
	svc := newService([]agentregistry.HarnessAgent{item}, nil, successfulModelDiscoverer())
	svc.SetAccountExecution(scopedAccountExecution{}, nil)
	for _, id := range []string{"worker-A", "orchestrator-B"} {
		got, err := svc.EnsureAgentAccountReadiness(context.Background(), "codex", domain.AgentReadinessPurposeLaunch, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Authentication.State != domain.AgentAuthenticationAuthorized || got.Authentication.Account == nil || got.Authentication.Account.Email != id+"@example.test" {
			t.Fatalf("readiness used another login: %+v", got)
		}
	}
	if adapter.calls.Load() != 0 {
		t.Fatal("managed readiness probed device credentials")
	}
}

func TestModelCatalogsAndFingerprintsUseSelectedAccount(t *testing.T) {
	discoverer := successfulModelDiscoverer()
	cache := &fakeModelCache{}
	svc := newService([]agentregistry.HarnessAgent{harnessAgent("codex", "Codex", nil)}, cache, discoverer)
	svc.SetAccountExecution(scopedAccountExecution{}, nil)
	for _, id := range []string{"worker-A", "reviewer-C"} {
		ctx := ports.WithAgentAccount(context.Background(), domain.ResolvedAgentAccount{AccountID: id, Provider: domain.AgentAccountProviderCodex})
		discoverer.mu.Lock()
		discoverer.catalog.Models = []ports.AgentModelInfo{{ID: id}}
		discoverer.mu.Unlock()
		got, err := svc.Models(ctx, "codex", "project", true)
		if err != nil || len(got.Models) == 0 || got.Models[0].ID != id {
			t.Fatalf("catalog=%+v error=%v", got, err)
		}
		discoverer.mu.Lock()
		request := discoverer.lastRequest
		discoverer.mu.Unlock()
		if request.Env["CODEX_HOME"] != "/fake/"+id || request.Env["AO_CODEX_MANAGED_ACCOUNT"] != "1" {
			t.Fatalf("discovery used another profile: %+v", request.Env)
		}
		if _, err := svc.CatalogFingerprint(ctx, "codex", "project"); err != nil {
			t.Fatal(err)
		}
		if request := discoverer.lastFingerprintRequest.Load(); request == nil || request.Env["CODEX_HOME"] != "/fake/"+id {
			t.Fatal("fingerprint used another profile")
		}
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	if len(cache.records) != 2 {
		t.Fatalf("account catalogs shared a cache key: %d", len(cache.records))
	}
}

func TestManagedModelErrorsNeverPersistProviderSecrets(t *testing.T) {
	marker := "fake-private-credential-marker"
	var logs bytes.Buffer
	discoverer := successfulModelDiscoverer()
	discoverer.err = fmt.Errorf("provider RPC failed: %s /fake/private/home", marker)
	cache := &fakeModelCache{}
	svc := newService([]agentregistry.HarnessAgent{harnessAgent("codex", "Codex", nil)}, cache, discoverer)
	svc.SetAccountExecution(scopedAccountExecution{}, nil)
	svc.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := ports.WithAgentAccount(context.Background(), domain.ResolvedAgentAccount{AccountID: "A", Provider: domain.AgentAccountProviderCodex})
	catalog, err := svc.Models(ctx, "codex", "", true)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), marker) || strings.Contains(logs.String(), marker) {
		t.Fatal("provider secret reached API/log")
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	for _, row := range cache.records {
		wire, _ := json.Marshal(row)
		if strings.Contains(string(wire), marker) || strings.Contains(string(wire), "/fake/private/home") {
			t.Fatal("provider secret/path reached DB cache")
		}
	}
	if _, err := svc.Models(ctx, "codex", ports.AccountModelCatalogScope("B"), false); !errors.Is(err, ports.ErrAgentAccountChanged) {
		t.Fatalf("context A borrowed B catalog: %v", err)
	}
}

type defaultScopedExecution struct{ scopedAccountExecution }

func (defaultScopedExecution) ResolveAccount(context.Context, ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	return domain.ResolvedAgentAccount{AccountID: "A", Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceGlobal}, nil
}
func TestModelWarmupUsesConfiguredAccountInsteadOfDevice(t *testing.T) {
	discoverer := successfulModelDiscoverer()
	cache := &fakeModelCache{}
	svc := newService([]agentregistry.HarnessAgent{harnessAgent("codex", "Codex", nil)}, cache, discoverer)
	svc.SetAccountExecution(defaultScopedExecution{}, nil)
	svc.prefetchModelCatalogs(context.Background(), true)
	discoverer.mu.Lock()
	defer discoverer.mu.Unlock()
	if discoverer.lastRequest.Env["CODEX_HOME"] != "/fake/A" {
		t.Fatalf("startup discovery used device: %+v", discoverer.lastRequest)
	}
}
