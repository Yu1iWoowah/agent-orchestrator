package agentaccounts

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestDefaultLaunchEnvKeepsTheChosenAccount(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "accounts")
	home := func(id string) string { return filepath.Join(root, "claude", id) }
	store := &memStore{
		accounts: []domain.AgentAccount{
			{ID: "team", Provider: domain.AgentAccountProviderClaude, Home: home("team"), Email: "team@example.com"},
			{ID: "max", Provider: domain.AgentAccountProviderClaude, Home: home("max"), Email: "max@example.com"},
		},
		defaults: map[domain.AgentAccountProvider]string{domain.AgentAccountProviderClaude: "team"},
		bindings: map[domain.SessionID]string{},
	}
	provider := &fakeClaude{signedInAs: map[string]string{home("team"): "team@example.com", home("max"): "max@example.com"}}
	svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, nil, nil)
	assertHome := func(want string) {
		t.Helper()
		env, err := svc.DefaultLaunchEnv(ctx, domain.HarnessClaudeCode)
		if err != nil || env["CLAUDE_CONFIG_DIR"] != want {
			t.Fatalf("discovery home = %q, error = %v; want %q", env["CLAUDE_CONFIG_DIR"], err, want)
		}
		if len(store.bindings) != 0 {
			t.Fatal("discovery bound a session")
		}
	}
	assertHome(home("team"))
	if err := svc.SetDefault(ctx, "max"); err != nil {
		t.Fatal(err)
	}
	assertHome(home("max"))
	delete(provider.signedInAs, home("max"))
	if _, err := svc.List(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DefaultLaunchEnv(ctx, domain.HarnessClaudeCode); !errors.Is(err, ports.ErrAgentAccountSignedOut) {
		t.Fatalf("signed-out configured default error = %v; must not select another login", err)
	}
	if store.defaults[domain.AgentAccountProviderClaude] != "max" {
		t.Fatal("discovery changed the configured default")
	}
}

func TestDefaultLaunchEnvLeavesComputerLoginUnchanged(t *testing.T) {
	svc := New(&memStore{}, []ports.AgentAccountProviderCLI{&fakeClaude{}}, t.TempDir(), nil, nil)
	for _, harness := range []domain.AgentHarness{domain.HarnessClaudeCode, domain.HarnessCodex, "other"} {
		env, err := svc.DefaultLaunchEnv(context.Background(), harness)
		if err != nil || len(env) != 0 {
			t.Fatalf("computer login env = %v, error = %v", env, err)
		}
	}
}

type fakeCodex struct{ fakeClaude }

func (*fakeCodex) Provider() domain.AgentAccountProvider { return domain.AgentAccountProviderCodex }

func TestCodexMetadataBindingDefersReadinessToPreflight(t *testing.T) {
	ctx := context.Background()
	store := &memStore{bindings: map[domain.SessionID]string{}}
	svc := New(store, []ports.AgentAccountProviderCLI{&fakeCodex{}}, t.TempDir(), nil, nil)
	if _, err := svc.List(ctx, true); err != nil {
		t.Fatal(err)
	}
	device := domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex)
	if err := svc.BindSession(ctx, "openai", domain.HarnessCodex, ""); err != nil || store.bindings["openai"] != device {
		t.Fatalf("metadata bind = %q, %v; want %q", store.bindings["openai"], err, device)
	}
	if _, err := svc.AccountExecutionEnv(ctx, domain.HarnessCodex, device); !errors.Is(err, ports.ErrAgentAccountSignedOut) {
		t.Fatalf("signed-out execution preflight = %v", err)
	}
}
