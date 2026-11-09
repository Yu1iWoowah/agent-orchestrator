package delegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/integration"
)

func deploymentFixture(t *testing.T, provider, project, source string) Deployment {
	t.Helper()
	if runtime.GOOS != "linux" || os.Getenv("HCG_PAL_TESTS") != "1" {
		t.Skip("isolated fixed PAL required")
	}
	venv := filepath.Dir(filepath.Dir(os.Getenv("HCG_TEST_PAL_PYTHON")))
	binary, err := os.ReadFile(filepath.Join(venv, "bin", "python"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	cfg, err := os.ReadFile(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	return Deployment{Profile: budgetProfile(provider), CredentialRef: CredentialReference{Provider: provider, Name: "synthetic-ref", Version: "v1"}, Workspace: "synthetic-workspace", Installation: Installation{Source: os.Getenv("HCG_TEST_PAL_SOURCE"), Venv: venv, Commit: PALCommit, SourceDigest: fixedPALSourceDigest, RuntimeDigest: "9b61fefab3c1b1da96f556c550be4773d0a31ce3b3bbb1fea3324d8fe57812c6", PythonDigest: hex.EncodeToString(sum[:]), ConfigDigest: textHash(string(cfg)), LockDigest: fixedDependencyLockDigest, PythonVersion: "3.12.3", AdapterVersion: "hcg-pal/1", TemplateVersion: resultSchema}, Inputs: []InputManifest{{ProjectID: project, Name: "synthetic-corpus", Revision: "r1", Files: []ApprovedFile{{Path: "prompt.txt", SHA256: textHash(source)}}}}}
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
func launchFixture(t *testing.T, c *Coordinator, id integration.Identity) {
	t.Helper()
	prepared, ok := c.CaptureLaunch(id, true, true)
	if !ok {
		t.Fatal("fresh metadata not captured")
	}
	if err := c.StoreLaunch(t.Context(), prepared, id); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorFixedRegistryPolicyAndCaller(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
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
			id := integration.Identity{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "worker", Mode: "chat", Harness: "codex", Worktree: worktree, LaunchID: "launch", NativeID: "native", ControllerGeneration: "generation"}
			if _, ok := c.CaptureLaunch(id, false, false); ok {
				t.Fatal("live reconnect fabricated missing launch history")
			}
			launchFixture(t, c, id)
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
			for _, unsupported := range []integration.Identity{
				{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "reviewer", Harness: "codex"},
				{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "worker", Harness: "other"},
			} {
				out := c.Read(t.Context(), unsupported, policy, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{"prompt.txt"}}, c.PolicyRevision(unsupported))
				if out.Reason != "unsupported" || calls != 1 {
					t.Fatalf("excluded caller dispatched: %+v", out)
				}
			}
			mount := c.Get(id, policy)
			disabled := false
			off := freezePolicy(policy)
			off.Project.Enabled = &disabled
			c.ProjectPolicyChanged("p", off)
			if err := c.SetOverride(t.Context(), id, nil); err != nil {
				t.Fatal(err)
			}
			got := c.Get(id, off)
			if got.EffectiveEnabled || !got.MountKnown || got.MountEnabled != mount.MountEnabled || *got.MountProfile != *mount.MountProfile {
				t.Fatalf("clear rewrote original mount: %+v", got)
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
			changed.Profile.Revision = "v2"
			approveFixture(t, c, changed)
			if view := c.Get(id, policy); view.Reason != "profile_revision_stale" || view.Active {
				t.Fatalf("old selection followed current implicitly: %+v", view)
			}
			next := delegationPolicy(changed.Profile)
			c.ProjectPolicyChanged("p", next)
			if view := c.Get(id, next); !view.Active || view.MountProfile.Revision != "v1" {
				t.Fatalf("new selection rewrote original mount: %+v", view)
			}
			liveID := id
			liveID.ControllerGeneration = "new-controller"
			prepared, ok := c.CaptureLaunch(liveID, false, false)
			if !ok {
				t.Fatal("live metadata not captured")
			}
			if err := c.StoreLaunch(t.Context(), prepared, liveID); err != nil {
				t.Fatal(err)
			}
			if c.Get(liveID, next).MountProfile.Revision != "v1" {
				t.Fatal("live reconnect replaced original launch profile")
			}
		})
	}
}

func TestCoordinatorStartupReceiptRecoveryNeverCallsProvider(t *testing.T) {
	for _, family := range []string{"openai", "anthropic"} {
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
			id := integration.Identity{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "worker", Mode: "chat", Harness: "codex", Worktree: worktree, LaunchID: "launch", NativeID: "native", ControllerGeneration: "generation"}
			launchFixture(t, c, id)
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
