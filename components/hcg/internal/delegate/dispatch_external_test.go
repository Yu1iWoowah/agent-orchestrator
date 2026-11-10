package delegate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/integration"
)

func TestDispatchRevalidatesSameDescriptorExternalSpend(t *testing.T) {
	for _, tc := range []struct {
		name                                                       string
		beforeOpenAI, beforeAnthropic, afterOpenAI, afterAnthropic int64
		unknown                                                    bool
		wantCalls                                                  int
	}{
		{"family_cap", 280, 0, 290, 0, false, 0},
		{"global_cap", 280, 330, 280, 340, false, 0},
		{"unknown", 280, 0, 280, 0, true, 0},
		{"exact_caps_count_own_hold_once", 280, 330, 280, 330, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, worktree := t.TempDir(), t.TempDir()
			body := "SYNTHETIC_SOURCE_142\nDOCUMENT_DIRECTIVE_IGNORE_THE_QUESTION\n"
			if err := os.WriteFile(filepath.Join(worktree, "prompt.txt"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			deployment := deploymentFixture(t, "openai", "p", body)
			spend := func(openai, anthropic int64, unknown bool) map[string]ExternalSpend {
				return map[string]ExternalSpend{"openai": {Known: !unknown, OutsideGateOnly: true, Nanos: openai * nanoUSD}, "anthropic": {Known: true, OutsideGateOnly: true, Nanos: anthropic * nanoUSD}}
			}
			sink := &receiptSink{}
			calls := 0
			var c *Coordinator
			grant := labGrantFixture(root, "p", "s", time.Now())
			id := integration.Identity{DataRoot: root, ProjectID: "p", SessionID: "s", Role: "worker", Mode: "chat", Harness: "codex", Worktree: worktree, LaunchID: "launch", NativeID: "native", ControllerGeneration: "generation"}
			c = NewCoordinator(root, sink, func(d Deployment) Transport {
				return transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
					calls++
					return providerFixtureReply(d.Profile.Model, validProposal(body)), nil
				})
			}, func(ctx context.Context, point string) {
				if point != "reserved" {
					return
				}
				before := c.PolicyRevision(id)
				if err := c.Register(ctx, Registration{Grant: grant, Deployment: deployment, ExternalSpend: spend(tc.afterOpenAI, tc.afterAnthropic, tc.unknown)}); err != nil {
					t.Fatal(err)
				}
				if c.PolicyRevision(id) != before {
					t.Fatal("fixture changed descriptor/revision instead of external knowledge only")
				}
			})
			defer c.Close()
			if err := c.Register(t.Context(), Registration{Grant: grant, Deployment: deployment, ExternalSpend: spend(tc.beforeOpenAI, tc.beforeAnthropic, false)}); err != nil {
				t.Fatal(err)
			}
			policy := delegationPolicy(deployment.Profile)
			c.ProjectPolicyChanged("p", policy)
			launchFixture(t, c, id)
			result := c.Read(t.Context(), id, policy, Request{Question: "QUESTION_MUST_SURVIVE", Paths: []string{"prompt.txt"}}, c.PolicyRevision(id))
			if calls != tc.wantCalls {
				t.Fatalf("transport calls=%d want=%d status=%s", calls, tc.wantCalls, result.Status)
			}
			if tc.wantCalls == 0 {
				if result.Status != "unavailable" || len(sink.receipts) != 0 {
					t.Fatalf("refused unsent request=%+v receipts=%d", result, len(sink.receipts))
				}
				for _, attempt := range readBudget(t, c.budget).Attempts {
					if attempt.State != "released" {
						t.Fatalf("unsent reservation not released: %s", attempt.State)
					}
				}
			} else if result.Status != "ok" {
				t.Fatalf("exact cap double-counted reservation: %+v", result)
			}
		})
	}
}
