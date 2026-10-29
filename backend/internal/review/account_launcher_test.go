package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
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

// These substitutes never open a real provider home or process.
type boundReviewerStore struct {
	ports.AgentAccountStore
	account domain.AgentAccount
	binding domain.ResolvedAgentAccount
}

func (s *boundReviewerStore) ListAgentAccounts(context.Context) ([]domain.AgentAccount, error) {
	return []domain.AgentAccount{s.account}, nil
}
func (s *boundReviewerStore) GetReviewerAgentAccount(context.Context, string, domain.AgentAccountProvider) (domain.ResolvedAgentAccount, bool, error) {
	return s.binding, true, nil
}
func (s *boundReviewerStore) BindReviewerAgentAccount(context.Context, string, domain.ResolvedAgentAccount, time.Time) error {
	return nil
}

type unusableReviewerClaude struct {
	*agentaccount.Claude
	signedOut bool
}

func (p *unusableReviewerClaude) Identity(context.Context, string) (domain.AgentAccountIdentityCheck, error) {
	if p.signedOut {
		return domain.AgentAccountIdentityCheck{}, nil
	}
	return domain.AgentAccountIdentityCheck{SignedIn: true, LoggedIn: true, Email: "wrong-person@example.invalid"}, nil
}
func (*unusableReviewerClaude) Limits(context.Context, string) (domain.AgentAccountLimits, error) {
	return domain.AgentAccountLimits{}, nil
}

type chatAccountReviewer struct{ fakeReviewer }

func (chatAccountReviewer) ReviewChatHarness() domain.AgentHarness { return domain.HarnessClaudeCode }

type countingAccountReviewChat struct {
	ReviewerChatController
	starts, restores int
}

func (*countingAccountReviewChat) SupportsReviewChat(domain.AgentHarness) bool { return true }
func (*countingAccountReviewChat) PreflightReviewChat(context.Context, domain.AgentHarness) error {
	return nil
}
func (c *countingAccountReviewChat) StartReviewChat(context.Context, ReviewerChatStart) (string, error) {
	c.starts++
	return "fake-conversation", nil
}
func (c *countingAccountReviewChat) RestoreReviewChat(context.Context, ReviewerChatStart) (string, error) {
	c.restores++
	return "fake-conversation", nil
}

func TestReviewerChatRefusesKnownUnusableBindingOnLaunchAndRestore(t *testing.T) {
	for _, signedOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "identity mismatch", true: "signed out"}[signedOut], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "accounts")
			home := filepath.Join(root, "claude", "C")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			provider := &unusableReviewerClaude{Claude: &agentaccount.Claude{DeviceHome: filepath.Join(root, "fake-device"), DeviceConfig: filepath.Join(root, "fake-device.json")}, signedOut: signedOut}
			binding := domain.ResolvedAgentAccount{AccountID: "C", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceRole}
			store := &boundReviewerStore{account: domain.AgentAccount{ID: "C", Provider: domain.AgentAccountProviderClaude, Home: home, ProfileKind: domain.AgentAccountProfileContained, Email: "C@example.invalid"}, binding: binding}
			accounts := agentaccounts.New(store, []ports.AgentAccountProviderCLI{provider}, root, nil, nil)
			if _, err := accounts.AccountExecutionEnv(context.Background(), domain.HarnessClaudeCode, "C"); !errors.Is(err, ports.ErrAgentAccountSignedOut) {
				t.Fatalf("exact-account control: %v", err)
			}
			chat := &countingAccountReviewChat{}
			launcher := NewLauncher(singleReviewerResolver{reviewer: &chatAccountReviewer{}}, &fakeRuntime{}, t.TempDir(), WithAgentAccounts(accounts), WithReviewerChat(chat))
			ctx := ports.WithAgentAccount(context.Background(), binding)
			if err := launcher.Preflight(ctx, domain.ReviewerClaudeCode, t.TempDir(), domain.ReviewerInterfaceChat); !errors.Is(err, ports.ErrAgentAccountSignedOut) {
				t.Errorf("Chat preflight accepted unusable binding: %v", err)
			}
			spec := launchSpec()
			spec.Harness = domain.ReviewerClaudeCode
			spec.ReviewSessionID = "review-C"
			if _, err := launcher.Spawn(ctx, spec); !errors.Is(err, ports.ErrAgentAccountSignedOut) {
				t.Errorf("Chat launch accepted unusable binding: %v", err)
			}
			if _, err := launcher.RestoreTerminal(context.Background(), spec); !errors.Is(err, ports.ErrAgentAccountSignedOut) {
				t.Errorf("Chat restore accepted unusable binding: %v", err)
			}
			if chat.starts != 0 || chat.restores != 0 {
				t.Fatalf("unusable identity reached provider: starts=%d restores=%d", chat.starts, chat.restores)
			}
		})
	}
}
