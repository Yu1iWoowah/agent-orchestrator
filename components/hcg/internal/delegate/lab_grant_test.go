package delegate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/integration"
)

func labGrantFixture(root, project, session string, now time.Time) *LabGrant {
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	owner := LabSession{ProjectID: project, SessionID: session}
	return &LabGrant{RunID: "synthetic-run", OwnerRoot: root, CanonicalOwner: owner,
		StartsAt: start, ExpiresAt: start.Add(24*time.Hour - time.Nanosecond),
		Profiles: map[string]ProfileRef{"openai": {ID: "openai-fixture", Revision: "v1"}, "anthropic": {ID: "anthropic-fixture", Revision: "v1"}},
		Bindings: []LabSession{owner}, ProviderCapNanos: map[string]int64{"openai": 10 * nanoUSD, "anthropic": 10 * nanoUSD}, TotalCapNanos: 20 * nanoUSD, AttemptsPerProvider: 26}
}

func TestLabRunRejectsWorstReservationAboveFamilyCap(t *testing.T) {
	root, worktree := t.TempDir(), t.TempDir()
	body := "public synthetic line\n"
	if err := os.WriteFile(filepath.Join(worktree, "prompt.txt"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	d := deploymentFixture(t, "openai", "p", body)
	d.Profile.Prices.InputPerMillion, d.Profile.Prices.CachedInputPerMillion = 200*nanoUSD, 200*nanoUSD
	calls := 0
	c := NewCoordinator(root, &receiptSink{}, func(Deployment) Transport {
		return transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
			calls++
			return providerFixtureReply(d.Profile.Model, validProposal(body)), nil
		})
	}, nil)
	defer c.Close()
	approveFixture(t, c, d)
	policy := delegationPolicy(d.Profile)
	c.ProjectPolicyChanged("p", policy)
	id := integration.Identity{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "worker", Mode: "chat", Harness: "codex", Worktree: worktree, LaunchID: "launch", NativeID: "native", ControllerGeneration: "generation"}
	launchFixture(t, c, id)
	out := c.Read(t.Context(), id, policy, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{"prompt.txt"}}, c.PolicyRevision(id))
	if out.Status != "unavailable" || calls != 0 {
		t.Fatalf("run cap failed: status=%s calls=%d", out.Status, calls)
	}
}

func TestLabRegistrationRequiresExplicitRunGrant(t *testing.T) {
	d := deploymentFixture(t, "openai", "p", "synthetic source")
	c := NewCoordinator(t.TempDir(), &receiptSink{}, nil, nil)
	defer c.Close()
	if err := c.Register(t.Context(), Registration{Deployment: d, ExternalSpend: map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}}); err == nil {
		t.Fatal("registration without an explicit run grant was accepted")
	}
}

func TestLabGrantIsBoundedAndAppendOnly(t *testing.T) {
	root := t.TempDir()
	g := labGrantFixture(root, "p", "s", time.Now())
	if !g.valid(root) || !g.current(time.Now()) {
		t.Fatal("fixed explicit grant rejected")
	}
	for _, change := range []func(*LabGrant){
		func(g *LabGrant) { g.OwnerRoot += "-other" },
		func(g *LabGrant) { g.TotalCapNanos++ },
		func(g *LabGrant) { g.ProviderCapNanos["openai"]++ },
		func(g *LabGrant) { g.AttemptsPerProvider++ },
		func(g *LabGrant) { g.ExpiresAt = g.StartsAt.Add(25 * time.Hour) },
		func(g *LabGrant) { g.Bindings = append(g.Bindings, g.Bindings[0]) },
	} {
		bad, _ := mergeLabGrant(nil, g)
		change(bad)
		if bad.valid(root) {
			t.Fatal("out-of-contract grant accepted")
		}
	}
	next, _ := mergeLabGrant(nil, g)
	next.Bindings = append(next.Bindings, LabSession{ProjectID: "p", SessionID: "s2"})
	if _, err := mergeLabGrant(g, next); err != nil {
		t.Fatal("explicit session binding rejected")
	}
	next.ExpiresAt = next.ExpiresAt.Add(-time.Hour)
	if _, err := mergeLabGrant(g, next); err == nil {
		t.Fatal("run deadline was mutable")
	}
}

func TestLabStartupRefusesMissingCanonicalDatabase(t *testing.T) {
	root := t.TempDir()
	if err := CheckLabStartup(t.Context(), root); err != nil {
		t.Fatal("new unregistered root rejected")
	}
	dir := filepath.Join(root, "hcg", "delegation")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckLabStartup(t.Context(), root); err == nil {
		t.Fatal("missing owner DB was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "ao.db")); !os.IsNotExist(err) {
		t.Fatal("startup check created a replacement DB")
	}
}
