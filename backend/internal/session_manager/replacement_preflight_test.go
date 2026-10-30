package sessionmanager

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementPreflightChat struct {
	*recordingLauncher
	resumeErr error
	checked   int
}

func (c *replacementPreflightChat) PreflightReplacementChat(ctx context.Context, harness domain.AgentHarness, permissions ports.PermissionMode) error {
	c.checked++
	if err := c.PreflightChat(ctx, harness, permissions); err != nil {
		return err
	}
	return c.resumeErr
}

type replacementPreflightAccounts struct {
	*transitionAccounts
	err     error
	checked int
}

func (a *replacementPreflightAccounts) ResolveAccount(context.Context, ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	return domain.ResolvedAgentAccount{AccountID: "acct-b", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceExplicit}, a.err
}
func (a *replacementPreflightAccounts) AccountExecutionEnv(context.Context, domain.AgentHarness, string) (map[string]string, error) {
	a.checked++
	return a.env("acct-b"), a.err
}

func TestReplacementTargetPreflightStartsNothingAndPinsAccount(t *testing.T) {
	chat := &replacementPreflightChat{recordingLauncher: &recordingLauncher{}}
	m, st, rt := newChatManager(chat)
	var events []string
	accounts := &replacementPreflightAccounts{transitionAccounts: newTransitionAccounts(&events)}
	m.SetAgentAccounts(accounts)
	agent := &launchAuthAgent{recordingAgent: &recordingAgent{}, status: ports.AgentAuthStatusAuthorized}
	m.agents = singleAgent{agent}
	project := st.projects["mer"]
	project.Path = t.TempDir()
	project.Config.Env = map[string]string{"CLAUDE_CONFIG_DIR": "wrong-project-account"}
	target, err := m.PreflightReplacementTarget(context.Background(), project)
	if err != nil || target.AccountID != "acct-b" || chat.checked != 1 || accounts.checked != 1 {
		t.Fatalf("target=%+v capability checks=%d accounts=%d err=%v", target, chat.checked, accounts.checked, err)
	}
	if agent.env["CLAUDE_CONFIG_DIR"] != "/accounts/b" || agent.workingDir != project.Path {
		t.Fatalf("auth probed another launch environment: %v path=%q", agent.env, agent.workingDir)
	}
	if rt.created != 0 || rt.destroyed != 0 || len(st.sessions) != 0 || len(chat.started) != 0 || len(chat.turns) != 0 || len(events) != 0 {
		t.Fatal("preflight spawned/bound a session or changed a runtime")
	}
}

func TestReplacementTargetPreflightRefusesUnavailablePrerequisites(t *testing.T) {
	for _, failure := range []string{"unsupported harness", "missing resume capability", "chat unavailable", "account unavailable", "account resolver missing", "unauthorized auth"} {
		t.Run(failure, func(t *testing.T) {
			chat := &replacementPreflightChat{recordingLauncher: &recordingLauncher{}}
			m, st, rt := newChatManager(chat)
			var events []string
			accounts := &replacementPreflightAccounts{transitionAccounts: newTransitionAccounts(&events)}
			m.SetAgentAccounts(accounts)
			project := st.projects["mer"]
			switch failure {
			case "unsupported harness":
				project.Config.Orchestrator.Harness = domain.HarnessCodex
			case "missing resume capability":
				chat.resumeErr = ports.ErrChatUnsupported
			case "chat unavailable":
				m.chat = nil
			case "account unavailable":
				accounts.err = errors.New("account is unavailable")
			case "account resolver missing":
				m.SetAgentAccounts(nil)
			case "unauthorized auth":
				m.agents = singleAgent{&launchAuthAgent{recordingAgent: &recordingAgent{}, status: ports.AgentAuthStatusUnauthorized}}
			}
			if _, err := m.PreflightReplacementTarget(context.Background(), project); err == nil {
				t.Fatal("unavailable target passed preflight")
			}
			if rt.created != 0 || len(st.sessions) != 0 || len(chat.started) != 0 || len(events) != 0 {
				t.Fatal("failed preflight changed sessions/runtime/accounts")
			}
		})
	}
}
