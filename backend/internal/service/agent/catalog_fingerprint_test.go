package agent

import (
	"context"
	"errors"
	"testing"

	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fingerprintBinaryAgent struct {
	fakeAgent
	binary string
	err    error
}

func (a *fingerprintBinaryAgent) ResolveBinary(context.Context) (string, error) {
	return a.binary, a.err
}

type fingerprintIdentityDiscoverer struct {
	*fakeModelDiscoverer
	identity string
}

func (d *fingerprintIdentityDiscoverer) CatalogFingerprint(_ context.Context, request ports.AgentModelDiscoveryRequest) string {
	d.lastFingerprintRequest.Store(&request)
	return request.Binary + ":" + d.identity
}

func TestCatalogFingerprintChecksCurrentInputsWithoutRefresh(t *testing.T) {
	ctx := context.Background()
	agent := &fingerprintBinaryAgent{binary: "binary-1"}
	discoverer := &fingerprintIdentityDiscoverer{fakeModelDiscoverer: &fakeModelDiscoverer{}, identity: "identity-A"}
	harness := harnessAgent("codex", "Codex", nil)
	harness.Agent = agent
	svc := newService([]agentregistry.HarnessAgent{harness}, &fakeModelCache{}, discoverer)
	svc.modelDiscoveryDir = t.TempDir()
	for _, tc := range []struct{ name, binary, identity, want string }{
		{"initial", "binary-1", "identity-A", "binary-1:identity-A"},
		{"identity changed", "binary-1", "identity-B", "binary-1:identity-B"},
		{"binary changed", "binary-2", "identity-A", "binary-2:identity-A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent.binary, discoverer.identity = tc.binary, tc.identity
			got, err := svc.CatalogFingerprint(ctx, "codex", "")
			if err != nil || got != tc.want {
				t.Fatalf("fingerprint=%q err=%v want=%q", got, err, tc.want)
			}
			request := discoverer.lastFingerprintRequest.Load()
			if request == nil || request.WorkingDir != svc.modelDiscoveryDir || request.AgentID != "codex" {
				t.Fatalf("discovery request=%+v", request)
			}
			if changed := svc.modelCatalogInputsChanged(ctx, "codex", "", "binary-1:identity-A"); changed != (tc.name != "initial") {
				t.Fatalf("background check disagrees: changed=%v", changed)
			}
		})
	}
	if discoverer.discoverCalls.Load() != 0 {
		t.Fatal("fingerprint check refreshed the catalog")
	}
	agent.err = errors.New("binary unavailable")
	if _, err := svc.CatalogFingerprint(ctx, "codex", ""); !errors.Is(err, agent.err) {
		t.Fatalf("resolver error lost: %v", err)
	}
	if svc.modelCatalogInputsChanged(ctx, "codex", "", "old") {
		t.Fatal("legacy background resolver-error behavior changed")
	}
	if _, err := svc.CatalogFingerprint(ctx, "unknown", ""); err == nil {
		t.Fatal("unknown adapter accepted")
	}
	agent.err = nil
	svc.modelDiscoveryDir = "relative"
	if _, err := svc.CatalogFingerprint(ctx, "codex", ""); err == nil {
		t.Fatal("unusable discovery inputs accepted")
	}
}
