package sessionmanager

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestRestorePassesResolvedAccountPolicyToNativeAndFreshCommands(t *testing.T) {
	for _, nativeID := range []string{"thread-1", ""} {
		t.Run(nativeID, func(t *testing.T) {
			agent := &recordingAgent{}
			env := map[string]string{agentlaunch.ManagedCodexAccountEnv: "1", "CODEX_HOME": "/fake/account"}
			_, _, _, err := restoreArgv(context.Background(), agent, "session-1", "/fake/workspace",
				domain.SessionMetadata{AgentSessionID: nativeID, Prompt: "fake task"}, "", "",
				domain.AgentConfig{}, domain.KindWorker, domain.HarnessCodex, "", env)
			if err != nil {
				t.Fatal(err)
			}
			if agent.lastRestore.Env[agentlaunch.ManagedCodexAccountEnv] != "1" {
				t.Fatal("native restore lost managed file-store policy")
			}
			if nativeID == "" && agent.lastLaunch.Env[agentlaunch.ManagedCodexAccountEnv] != "1" {
				t.Fatal("fresh fallback lost managed file-store policy")
			}
		})
	}
}

func TestAccountPolicyCannotComeFromProjectOrInheritedEnvironment(t *testing.T) {
	for _, caseInsensitive := range []bool{false, true} {
		manager := &Manager{}
		env := map[string]string{agentlaunch.ManagedCodexAccountEnv: "1"}
		if caseInsensitive {
			env["ao_codex_managed_account"] = "1"
		}
		if err := manager.applyAgentAccountEnv("session-1", "", env, caseInsensitive); err != nil {
			t.Fatal(err)
		}
		if env[agentlaunch.ManagedCodexAccountEnv] != "" || (caseInsensitive && env["ao_codex_managed_account"] != "") {
			t.Fatal("unbound account kept an untrusted managed policy")
		}
	}
}

type managedCodexPolicyAccounts struct{ *transitionAccounts }

func (*managedCodexPolicyAccounts) SessionLaunchEnv(context.Context, domain.SessionID) (map[string]string, error) {
	return map[string]string{agentlaunch.ManagedCodexAccountEnv: "1", "CODEX_HOME": "/fake/account"}, nil
}

func TestSpawnPassesResolvedAccountPolicyToCommand(t *testing.T) {
	manager, _, _, _ := newManager()
	agent := &recordingAgent{}
	manager.agents = singleAgent{agent: agent}
	var log []string
	manager.SetAgentAccounts(&managedCodexPolicyAccounts{newTransitionAccounts(&log)})
	_, _, _, err := manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: "mer", Kind: domain.KindWorker, AgentAccountID: "fake-managed-account",
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.lastLaunch.Env[agentlaunch.ManagedCodexAccountEnv] != "1" {
		t.Fatal("spawn command lost resolved managed file-store policy")
	}
}
