package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

)

func deploymentFixture(t *testing.T, provider, _, _ string) Deployment {
	t.Helper()
	profile := budgetProfile(provider)
	profile.Prices.InputPerMillion, profile.Prices.CachedInputPerMillion = 50*nanoUSD, 50*nanoUSD
	return Deployment{Profile: profile, CredentialRef: CredentialReference{Provider: provider, Name: "synthetic-ref", Version: "v1"}, Workspace: "synthetic-workspace"}
}
func delegationPolicy(p Profile) PolicySnapshot {
	enabled := true
	ref := ProfileRef{p.ID, p.Revision}
	return PolicySnapshot{Project: ProjectPolicy{Enabled: &enabled, AllowedProfiles: []ProfileRef{ref}, SelectedProfile: &ref}}
}

func approveFixture(t *testing.T, c *Coordinator, d Deployment) {
	t.Helper()
	if err := c.Register(t.Context(), Registration{Deployment: d, ExternalSpend: map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}}); err != nil {
		t.Fatal(err)
	}
}
func TestCoordinatorFixedRegistryPolicyAndCaller(t *testing.T) {
	for _, provider := range []string{"openai"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			worktree := t.TempDir()
			body := "SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"
			if err := os.WriteFile(filepath.Join(worktree, "prompt.txt"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			deployment := deploymentFixture(t, provider, "p", body)
			sink := &receiptSink{}
			calls := 0
			c := NewCoordinator(root, sink, func(d Deployment) Transport {
				return transportFunc(func(ctx context.Context, r WireRequest) (WireResponse, error) {
					calls++
					return (&captureTransport{t: t, profile: d.Profile}).Do(ctx, r)
				})
			}, nil)
			defer c.Close()
			approveFixture(t, c, deployment)
			policy := delegationPolicy(deployment.Profile)
			c.ProjectPolicyChanged("p", policy)
			id := Identity{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "worker", Mode: "chat", Harness: "codex", Worktree: worktree}
			token := c.PolicyRevision(id)
			out := c.Read(t.Context(), id, policy, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{"prompt.txt"}}, token)
			if out.Status != "ok" || out.BillingProvider != provider || out.ProjectID != "p" || out.SessionID != "s" || calls != 1 || len(sink.receipts) != 1 {
				t.Fatalf("coordinator caller failed: %+v calls=%d", out, calls)
			}
			offValue := false
			roleOff := freezePolicy(policy)
			roleOff.Worker = &offValue
			projectOff := freezePolicy(policy)
			projectOff.Project.Enabled = &offValue
			unset := freezePolicy(policy)
			unset.Project.SelectedProfile = nil
			ambiguous := freezePolicy(policy)
			ambiguous.Project.AllowedProfiles = append(ambiguous.Project.AllowedProfiles, ambiguous.Project.AllowedProfiles[0])
			notAllowed := freezePolicy(policy)
			notAllowed.Project.SelectedProfile = &ProfileRef{ID: "not-allowed", Revision: "v1"}
			unregistered := freezePolicy(policy)
			unregistered.Project.SelectedProfile = &ProfileRef{ID: "missing", Revision: "v1"}
			unregistered.Project.AllowedProfiles = []ProfileRef{*unregistered.Project.SelectedProfile}
			for _, tc := range []struct {
				policy PolicySnapshot
				reason string
			}{
				{projectOff, "project_opt_in_required"}, {roleOff, "integration_disabled"}, {unset, "profile_unset"}, {ambiguous, "profile_ambiguous"}, {notAllowed, "profile_not_allowed"}, {unregistered, "profile_unregistered"},
			} {
				c.ProjectPolicyChanged("p", tc.policy)
				out := c.Read(t.Context(), id, tc.policy, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{"prompt.txt"}}, c.PolicyRevision(id))
				if out.Status != "unavailable" || out.Reason != tc.reason || calls != 1 {
					t.Fatalf("invalid profile/policy dispatched: reason=%s reply=%+v calls=%d", tc.reason, out, calls)
				}
			}
			c.ProjectPolicyChanged("p", policy)
			for _, unsupported := range []Identity{
				{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "reviewer", Harness: "codex"},
				{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "worker", Harness: "other"},
			} {
				out := c.Read(t.Context(), unsupported, policy, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{"prompt.txt"}}, c.PolicyRevision(unsupported))
				if out.Reason != "unsupported" || calls != 1 {
					t.Fatalf("excluded caller dispatched: %+v", out)
				}
			}
			disabled := false
			off := freezePolicy(policy)
			off.Project.Enabled = &disabled
			c.ProjectPolicyChanged("p", off)
			if err := c.SetOverride(t.Context(), id, nil); err != nil {
				t.Fatal(err)
			}
			if got := c.Get(id, off); got.EffectiveEnabled {
				t.Fatalf("project off still enabled: %+v", got)
			}
			on := true
			if err := c.SetOverride(t.Context(), id, &on); err != nil {
				t.Fatal(err)
			}
			if c.Get(id, off).EffectiveEnabled {
				t.Fatal("session on bypassed project opt-in")
			}
			c.ProjectPolicyChanged("p", policy)
			if err := c.SetOverride(t.Context(), id, nil); err != nil {
				t.Fatal(err)
			}
			// Project, host and session ABA each invalidate the old unregistered token.
			for _, change := range []func(){
				func() { c.ProjectPolicyChanged("p", off); c.ProjectPolicyChanged("p", policy) },
				func() {
					if err := c.EmergencyStop(t.Context(), true); err != nil {
						t.Fatal(err)
					}
					if err := c.EmergencyStop(t.Context(), false); err != nil {
						t.Fatal(err)
					}
				},
				func() {
					if err := c.SetOverride(t.Context(), id, &disabled); err != nil {
						t.Fatal(err)
					}
					if err := c.SetOverride(t.Context(), id, &on); err != nil {
						t.Fatal(err)
					}
				},
			} {
				old := c.PolicyRevision(id)
				change()
				result := c.Read(t.Context(), id, policy, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{"prompt.txt"}}, old)
				if result.Status != "unavailable" || result.Reason != "policy_revision_changed" || calls != 1 {
					t.Fatalf("ABA admitted old request: %+v calls=%d", result, calls)
				}
			}
			changed := deployment
			changed.Profile.Prices.Version = "different-price"
			if err := c.Register(t.Context(), Registration{Deployment: changed, ExternalSpend: map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}}); err == nil {
				t.Fatal("immutable revision overwritten")
			}
		})
	}
}

func TestCoordinatorStartupReceiptRecoveryNeverCallsProvider(t *testing.T) {
	for _, family := range []string{"openai"} {
		t.Run(family, func(t *testing.T) {
			body := "SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"
			root, worktree := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(worktree, "prompt.txt"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			deployment := deploymentFixture(t, family, "p", body)
			sink := &receiptSink{err: errors.New("synthetic canonical unavailable")}
			calls := 0
			factory := func(d Deployment) Transport {
				return transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
					calls++
					return providerFixtureReply(d.Profile.Model, validProposal(body)), nil
				})
			}
			c := NewCoordinator(root, sink, factory, nil)
			approveFixture(t, c, deployment)
			policy := delegationPolicy(deployment.Profile)
			c.ProjectPolicyChanged("p", policy)
			id := Identity{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "worker", Mode: "chat", Harness: "codex", Worktree: worktree}
			reply := c.Read(t.Context(), id, policy, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{"prompt.txt"}}, c.PolicyRevision(id))
			if reply.Status != "unavailable" || calls != 1 {
				t.Fatalf("failed canonical write leaked answer: %+v calls=%d", reply, calls)
			}
			c.Close()
			sink.err = nil
			restarted := NewCoordinator(root, sink, factory, nil)
			defer restarted.Close()
			if err := restarted.RecoverReceipts(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(sink.receipts) != 1 || sink.receipts[0].ProfileRevision != "v1" || sink.receipts[0].Charges.PriceVersion != "price-v1" {
				t.Fatalf("startup retried provider or lost immutable receipt: calls=%d receipts=%+v", calls, sink.receipts)
			}
			if err := restarted.RecoverReceipts(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(sink.receipts) != 1 {
				t.Fatal("receipt recovery was not idempotent")
			}
			state := readBudget(t, restarted.budget)
			for _, attempt := range state.Attempts {
				if attempt.State != "settled" {
					t.Fatalf("successful recovery lost settlement: %+v", attempt)
				}
			}
		})
	}
}
