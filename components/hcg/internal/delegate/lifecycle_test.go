package delegate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	// The fixture approves this installation once, including path-dependent
	// RECORD bytes. Production still verifies the entire snapshot on each use.
	runtimeDigest, err := snapshotTree(t.Context(), filepath.Join(venv, "lib", "python3.12", "site-packages"), "", false)
	if err != nil {
		t.Fatal(err)
	}
	profile := budgetProfile(provider)
	profile.Prices.InputPerMillion, profile.Prices.CachedInputPerMillion = 50*nanoUSD, 50*nanoUSD
	return Deployment{Profile: profile, CredentialRef: CredentialReference{Provider: provider, Name: "synthetic-ref", Version: "v1"}, Workspace: "synthetic-workspace", Installation: Installation{Source: os.Getenv("HCG_TEST_PAL_SOURCE"), Venv: venv, Commit: PALCommit, SourceDigest: fixedPALSourceDigest, RuntimeDigest: runtimeDigest, PythonDigest: hex.EncodeToString(sum[:]), ConfigDigest: textHash(string(cfg)), LockDigest: fixedDependencyLockDigest, PythonVersion: "3.12.3", AdapterVersion: "hcg-pal/1", TemplateVersion: resultSchema}, Inputs: []InputManifest{{ProjectID: project, Name: "synthetic-corpus", Revision: "r1", Files: []ApprovedFile{{Path: "prompt.txt", SHA256: textHash(source)}}}}}
}
func delegationPolicy(p Profile) PolicySnapshot {
	enabled := true
	ref := ProfileRef{p.ID, p.Revision}
	return PolicySnapshot{Project: ProjectPolicy{Enabled: &enabled, AllowedProfiles: []ProfileRef{ref}, SelectedProfile: &ref}}
}

func TestFixtureRuntimeApprovalStillRejectsChangedBytes(t *testing.T) {
	d := deploymentFixture(t, "openai", "p", "synthetic source")
	if err := verifyInstallation(t.Context(), d.Installation); err != nil {
		t.Fatalf("fresh fixture installation rejected: %v", err)
	}
	// Snapshot into our own directory; never alter the supplied installation.
	venv := t.TempDir()
	site := filepath.Join(venv, "lib", "python3.12", "site-packages")
	if err := os.MkdirAll(site, 0700); err != nil {
		t.Fatal(err)
	}
	digest, err := snapshotTree(t.Context(), filepath.Join(d.Installation.Venv, "lib", "python3.12", "site-packages"), site, false)
	if err != nil || digest != d.Installation.RuntimeDigest {
		t.Fatalf("copy changed approved runtime digest: %v", err)
	}
	if err := os.Mkdir(filepath.Join(venv, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pyvenv.cfg", "bin/python"} {
		body, err := os.ReadFile(filepath.Join(d.Installation.Venv, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(venv, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	d.Installation.Venv = venv
	if err := verifyInstallation(t.Context(), d.Installation); err != nil {
		t.Fatalf("unchanged private snapshot rejected: %v", err)
	}
	// A RECORD row is part of the approval, even though its script hashes can
	// legitimately differ when initially installed at another absolute path.
	records, err := filepath.Glob(filepath.Join(site, "*.dist-info", "RECORD"))
	if err != nil || len(records) == 0 {
		t.Fatal("fixture has no RECORD files")
	}
	file, err := os.OpenFile(records[0], os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("\nsynthetic-post-approval-change\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("mutate private fixture: %v %v", writeErr, closeErr)
	}
	if err := verifyInstallation(t.Context(), d.Installation); err == nil {
		t.Fatal("post-approval runtime RECORD change bypassed gate")
	}
}
func approveFixture(t *testing.T, c *Coordinator, d Deployment) {
	t.Helper()
	g := c.LabGrant()
	if g == nil {
		g = labGrantFixture(c.root, d.Inputs[0].ProjectID, "s", time.Now())
	}
	if err := c.Register(t.Context(), Registration{Grant: g, Deployment: d, ExternalSpend: map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}}); err != nil {
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
			if err := c.Register(t.Context(), Registration{Grant: c.LabGrant(), Deployment: changed, ExternalSpend: map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}}); err == nil {
				t.Fatal("immutable revision overwritten")
			}
			changed.Profile.Revision = "v2"
			if err := c.Register(t.Context(), Registration{Grant: c.LabGrant(), Deployment: changed, ExternalSpend: map[string]ExternalSpend{"openai": {Known: true, OutsideGateOnly: true}, "anthropic": {Known: true, OutsideGateOnly: true}}}); err == nil {
				t.Fatal("run accepted an ungranted revision")
			}
			next := delegationPolicy(changed.Profile)
			c.ProjectPolicyChanged("p", next)
			if c.Get(id, next).Active {
				t.Fatal("ungranted revision became active")
			}
			c.ProjectPolicyChanged("p", policy)
			next = policy
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
			for _, profile := range restarted.Profiles() {
				if profile.Available {
					t.Fatal("startup recovery enabled transport without explicit reauthorization")
				}
			}
			grant := restarted.LabGrant()
			restarted.budget.Now = func() time.Time { return grant.ExpiresAt.Add(time.Second) }
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

// Five representatives on the one production caller: the attempt's verbatim
// body, private refusal point and canonical association are recorded, while
// the public reply keeps its original shape and never carries raw text.
func TestReadRecordsProviderReplyAtSameCaller(t *testing.T) {
	source := "SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"
	marker := "SYNTHETIC_TRANSPORT_MARKER" // synthetic credential-echo stand-in
	rejected := strings.Replace(validProposal(source), `"source_id":"s0000"`, `"source_id":"s9999"`, 1)
	echo := []byte(`{"choices":[{"message":{"content":"` + marker + `"}}]}` + "\r\n")
	for _, tc := range []struct {
		name, status, reason, state, validation, code string
		respond                                       func(model string) (WireResponse, error)
		failWrite                                     bool
	}{
		{"accepted", "ok", "machine_checked_locations_only", "sent_validation_done", "accepted", "", func(m string) (WireResponse, error) {
			return providerFixtureReply(m, validProposal(source)), nil
		}, false},
		{"not_found", "not_found", "not_found_in_injected_scope", "sent_validation_done", "not_found", "", func(m string) (WireResponse, error) {
			return providerFixtureReply(m, `{"schema":"hcg.delegate-read/1","status":"not_found","findings":[],"evidence":[]}`), nil
		}, false},
		{"verifier_rejected", "blocked", "unverified", "sent_validation_done", "rejected", "evidence_source_unknown", func(m string) (WireResponse, error) {
			return providerFixtureReply(m, rejected), nil
		}, false},
		{"parent_rejected", "unavailable", "integration_unavailable", "sent_validation_not_run", "not_run", "transport_failed", func(string) (WireResponse, error) {
			return WireResponse{Status: 200, Headers: map[string]string{"x-request-id": "fixture-request"}, Capture: &WireCapture{Outcome: "response", Body: echo, Complete: true, ObservedAtLeast: int64(len(echo)), Stage: "credential_scan", Code: "credential_echo"}}, errors.New("synthetic parent refusal")
		}, false},
		{"record_failed", "unavailable", "integration_unavailable", "record_failed", "", "", func(m string) (WireResponse, error) {
			return providerFixtureReply(m, validProposal(source)), nil
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, worktree := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(worktree, "prompt.txt"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			d := deploymentFixture(t, "openai", "p", source)
			sink := &receiptSink{}
			calls := 0
			var sent WireResponse
			c := NewCoordinator(root, sink, func(d Deployment) Transport {
				return transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
					calls++
					response, err := tc.respond(d.Profile.Model)
					sent = response
					return response, err
				})
			}, nil)
			defer c.Close()
			if tc.failWrite {
				// Fail only the terminal record written after verification, so a
				// swallowed finalize error would deliver a normal ok answer.
				writer := c.replies.write
				c.replies.write = func(dir, name string, data []byte) error {
					if name == "record.json" && bytes.Contains(data, []byte(`"state":"sent_validation_done"`)) {
						return errors.New("synthetic disk full")
					}
					return writer(dir, name, data)
				}
			}
			approveFixture(t, c, d)
			policy := delegationPolicy(d.Profile)
			c.ProjectPolicyChanged("p", policy)
			id := integration.Identity{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "worker", Mode: "chat", Harness: "codex", Worktree: worktree, LaunchID: "launch", NativeID: "native", ControllerGeneration: "generation"}
			launchFixture(t, c, id)
			out := c.Read(t.Context(), id, policy, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{"prompt.txt"}}, c.PolicyRevision(id))
			public, _ := json.Marshal(out)
			if out.Status != tc.status || out.Reason != tc.reason || calls != 1 || strings.Contains(string(public), marker) || (tc.code != "" && strings.Contains(string(public), tc.code)) {
				t.Fatalf("public reply changed or leaked: %s calls=%d", public, calls)
			}
			record, body, _ := readReply(t, c.replies, out.UsageRef.AttemptID)
			wantBody := sent.Body
			if sent.Capture != nil {
				wantBody = sent.Capture.Body
			}
			budget := readBudget(t, c.budget)
			attempt, known := budget.Attempts[record.Identity.BudgetAttemptKey]
			i := record.Identity
			if record.State != tc.state || !known || i.ProjectID != "p" || i.SessionID != "s" || i.AttemptID != out.UsageRef.AttemptID || i.DelegationID != out.DelegationID || i.RunID != "synthetic-run" || i.ProfileID != d.Profile.ID || i.ProfileDigest != out.UsageRef.ProfileDigest || attempt.Execution.AttemptID != i.AttemptID {
				t.Fatalf("record not bound to the original attempt: %+v", record)
			}
			if tc.failWrite {
				if record.RecordFailure != "terminal_write_failed" || len(sink.receipts) != 1 {
					t.Fatalf("record failure/accounting: %+v receipts=%d", record, len(sink.receipts))
				}
				again := c.Read(t.Context(), id, policy, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{"prompt.txt"}}, c.PolicyRevision(id))
				if again.Status != "unavailable" || calls != 1 {
					t.Fatalf("frozen owner sent again: %+v calls=%d", again, calls)
				}
				return
			}
			if !bytes.Equal(body, wantBody) || record.Capture == nil || !record.Capture.Complete || record.Receipt == nil || record.Receipt.ReceiptSHA256 != attempt.ReceiptDigest || record.Validation == nil || record.Validation.Result != tc.validation {
				t.Fatalf("verbatim body/receipt/validation lost: %+v", record)
			}
			if tc.code != "" && (record.Validation.Failure == nil || record.Validation.Failure.Code != tc.code) {
				t.Fatalf("private first failure: %+v", record.Validation.Failure)
			}
			if tc.name == "parent_rejected" && (record.Capture.ParentRefusal == nil || record.Capture.ParentRefusal.Code != "credential_echo" || !bytes.Contains(body, []byte(marker))) {
				t.Fatalf("parent refusal body not kept verbatim: %+v", record.Capture)
			}
		})
	}
}
