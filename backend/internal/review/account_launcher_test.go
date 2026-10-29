package review

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fixedReviewerAccount struct {
	ports.ReviewerAccountBindings
	env map[string]string
	err error
}

func (a fixedReviewerAccount) ReviewerLaunchEnv(_ context.Context, id string, _ domain.AgentHarness) (map[string]string, error) {
	if id != "review-1" {
		return nil, ports.ErrAgentAccountUnknown
	}
	return a.env, a.err
}
func TestReviewerAccountEnvOverridesProjectAndAdapterCredentials(t *testing.T) {
	home := t.TempDir()
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "isolated", true: "unsafe refuses launch"}[fail], func(t *testing.T) {
			adapter := &fakeReviewer{env: map[string]string{"CLAUDE_CONFIG_DIR": "/fake/adapter-device", "ANTHROPIC_API_KEY": "fake-adapter-secret"}}
			rt := &fakeRuntime{}
			accounts := fixedReviewerAccount{env: (&agentaccount.Claude{}).LaunchEnv(home)}
			if fail {
				accounts.err = ports.ErrAgentAccountStorageUnsafe
			}
			launcher := NewLauncher(fakeReviewerResolver{reviewer: adapter, ok: true}, rt, t.TempDir(), WithAgentAccounts(accounts))
			spec := launchSpec()
			spec.ProjectEnv = map[string]string{"CLAUDE_CONFIG_DIR": "/fake/project-device", "CLAUDE_SECURESTORAGE_CONFIG_DIR": "/fake/other", "ANTHROPIC_AUTH_TOKEN": "fake-project-secret", "KEEP": "yes"}
			_, err := launcher.Spawn(context.Background(), spec)
			if fail {
				if !errors.Is(err, ports.ErrAgentAccountStorageUnsafe) || len(rt.createCfg.Argv) != 0 {
					t.Fatalf("unsafe reviewer started: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if adapter.gotInv.Env["CLAUDE_CONFIG_DIR"] != home {
				t.Fatal("review command used worker/device context")
			}
			env := rt.createCfg.Env
			if env["CLAUDE_CONFIG_DIR"] != home || env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != home || env["ANTHROPIC_API_KEY"] != "" || env["ANTHROPIC_AUTH_TOKEN"] != "" || env["KEEP"] != "yes" {
				t.Fatalf("reviewer isolation failed: %+v", env)
			}
		})
	}
}
