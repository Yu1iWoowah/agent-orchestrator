package agentaccounts

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type handoffLimitsProvider struct {
	*fakeClaude
	used     map[string]float64
	onLimits func()
}

func (p *handoffLimitsProvider) Limits(_ context.Context, home string) (domain.AgentAccountLimits, error) {
	if p.onLimits != nil {
		p.onLimits()
	}
	return domain.AgentAccountLimits{Windows: []domain.AgentAccountLimitWindow{
		{ID: "five_hour", UsedPercent: p.used[home]},
		{ID: "seven_day_opus", UsedPercent: 100, Scope: "opus"},
	}}, nil
}

func TestHandoffTargetPicksMostVerifiedRoomForTheModel(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	home := func(id string) string { return filepath.Join(root, "claude", id) }
	store := &memStore{
		accounts: []domain.AgentAccount{
			{ID: "a", Provider: domain.AgentAccountProviderClaude, Home: home("a"), Email: "a@example.com"},
			{ID: "b", Provider: domain.AgentAccountProviderClaude, Home: home("b"), Email: "b@example.com"},
			{ID: "c", Provider: domain.AgentAccountProviderClaude, Home: home("c"), Email: "c@example.com"},
			{ID: "full", Provider: domain.AgentAccountProviderClaude, Home: home("full"), Email: "full@example.com"},
			{ID: "out", Provider: domain.AgentAccountProviderClaude, Home: home("out"), Email: "out@example.com"},
			{ID: "other", Provider: domain.AgentAccountProviderClaude, Home: home("other"), Email: "other@example.com"},
		},
		defaults: map[domain.AgentAccountProvider]string{},
		bindings: map[domain.SessionID]string{"session": "a"},
	}
	provider := &handoffLimitsProvider{
		fakeClaude: &fakeClaude{
			signedInAs: map[string]string{
				home("a"): "a@example.com", home("b"): "b@example.com", home("c"): "c@example.com",
				home("full"): "full@example.com", home("other"): "someone-else@example.com",
			},
			failing: map[string]bool{},
		},
		used: map[string]float64{home("a"): 0, home("b"): 50, home("c"): 20, home("full"): 95, home("out"): 0, home("other"): 0},
	}
	svc := New(store, []ports.AgentAccountProviderCLI{provider}, root, nil, nil)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		model string
		used  map[string]float64
		want  string
	}{
		{"most room wins", "claude-sonnet-5", map[string]float64{home("b"): 50, home("c"): 20, home("full"): 95}, "c"},
		{"a nearly full account is picked when nothing has more room", "claude-sonnet-5", map[string]float64{home("b"): 100, home("c"): 100, home("full"): 95}, "full"},
		{"the model's full meter rules every account out", "claude-opus-5", map[string]float64{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			maps.Copy(provider.used, tc.used)
			got, err := svc.HandoffTarget(ctx, "session", domain.HarnessClaudeCode, tc.model)
			if err != nil || got != tc.want {
				t.Fatalf("HandoffTarget = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := svc.HandoffTarget(ctx, "session", domain.HarnessCodex, ""); err == nil {
		t.Fatal("a harness without this provider picked a target")
	}
	var moved sync.Once
	provider.onLimits = func() { moved.Do(func() { store.bindings["session"] = "b" }) }
	if got, err := svc.HandoffTarget(ctx, "session", domain.HarnessClaudeCode, "claude-sonnet-5"); !errors.Is(err, ports.ErrAgentAccountChanged) {
		t.Fatalf("a move during the probes: target %q, err %v", got, err)
	}
}
