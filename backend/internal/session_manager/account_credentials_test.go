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
		if err := manager.applyAgentAccountEnv("session-1", "", env, caseInsensitive, domain.HarnessCodex); err != nil {
			t.Fatal(err)
		}
		if env[agentlaunch.ManagedCodexAccountEnv] != "" || (caseInsensitive && env["ao_codex_managed_account"] != "") {
			t.Fatal("unbound account kept an untrusted managed policy")
		}
	}
}

type managedCodexPolicyAccounts struct{ *transitionAccounts }

func (*managedCodexPolicyAccounts) SessionLaunchEnv(context.Context, domain.SessionID, domain.AgentHarness) (map[string]string, error) {
	return map[string]string{agentlaunch.ManagedCodexAccountEnv: "1", "CODEX_HOME": "/fake/account"}, nil
}

func TestSpawnPassesResolvedAccountPolicyToCommand(t *testing.T) {
	manager, store, runtime, _ := newManager()
	project := store.projects["mer"]
	project.Config.Env = map[string]string{}
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "fake-ambient-credential")
		project.Config.Env[key] = "fake-project-credential"
	}
	store.projects["mer"] = project
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
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		if agent.lastLaunch.Env[key] != "" || runtime.lastCfg.Env[key] != "" {
			t.Errorf("managed TUI launch received %s", key)
		}
	}
	if agent.lastLaunch.Env[agentlaunch.ManagedCodexAccountEnv] != "1" {
		t.Fatal("spawn command lost resolved managed file-store policy")
	}
}
