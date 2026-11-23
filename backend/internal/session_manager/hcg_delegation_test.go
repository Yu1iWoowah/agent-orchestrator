package sessionmanager

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type delegationMountObserver struct{ mounts []ports.HCGDelegationMount }

func (o *delegationMountObserver) DelegationMountRequested(m ports.HCGDelegationMount) bool {
	o.mounts = append(o.mounts, m)
	return true
}

func TestHCGDelegationChatMountFreshAndRestore(t *testing.T) {
	launcher := &recordingLauncher{}
	mgr, store, _ := newChatManager(launcher)
	mgr.dataDir = t.TempDir()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	binDir := t.TempDir()
	executable := filepath.Join(binDir, "ao"+ext)
	helper := filepath.Join(binDir, "ao-mcp"+ext)
	for _, binary := range []string{executable, helper} {
		if err := os.WriteFile(binary, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mgr.executable = func() (string, error) { return executable, nil }
	mgr.hcgDelegation = &delegationMountObserver{}
	rec, _, _, err := mgr.Spawn(t.Context(), ports.SpawnConfig{ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex, RequestedMode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	stored := store.sessions[rec.ID]
	stored.IsTerminated = true
	stored.Metadata.ProviderConversationID = "synthetic-native"
	store.sessions[rec.ID] = stored
	if _, err := mgr.RestoreWithMode(t.Context(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if len(launcher.started) != 2 {
		t.Fatalf("starts=%d", len(launcher.started))
	}
	for _, start := range launcher.started {
		if len(start.MCPServers) != 2 || start.MCPServers[0].Name != "ao" || start.MCPServers[1].Name != "hcg" {
			t.Fatalf("MCP identity: %+v", start.MCPServers)
		}
		ao := start.MCPServers[0]
		if ao.Command != helper || len(ao.Args) != 0 {
			t.Fatalf("ao MCP command = %q %v, want %q with no args", ao.Command, ao.Args, helper)
		}
		hcg := start.MCPServers[1]
		if hcg.Command != executable {
			t.Fatalf("HCG MCP command = %q, want daemon executable %q", hcg.Command, executable)
		}
		if len(hcg.Args) != 3 || hcg.Args[0] != "usage" || hcg.Args[1] != "delegation" || hcg.Args[2] != "mcp" || hcg.Env[EnvSessionID] != string(rec.ID) {
			t.Fatalf("fixed HCG config=%+v", hcg)
		}
		for key := range hcg.Env {
			if key != EnvSessionID && key != EnvDataDir && key != EnvRunFile {
				t.Fatalf("unexpected forwarded env name: %s", key)
			}
		}
	}
	if got := mgr.chatMCPServers(t.Context(), domain.HarnessCodex, domain.SessionKind("reviewer"), "review", "p", map[string]string{}); len(got) != 1 || got[0].Name != "ao" {
		t.Fatal("reviewer received delegation")
	}
}

func TestHCGDelegationTUIMountSuccessAndRestore(t *testing.T) {
	mgr, store, _, ws := newManager()
	ws.path = t.TempDir()
	mgr.dataDir = t.TempDir()
	observer := &delegationMountObserver{}
	mgr.hcgDelegation = observer
	rec, _, _, err := mgr.Spawn(t.Context(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessCodex})
	if err != nil {
		t.Fatal(err)
	}
	stored := store.sessions[rec.ID]
	stored.IsTerminated = true
	stored.Metadata.AgentSessionID = "synthetic-native"
	stored.Metadata.AgentSessionIDLaunchID = stored.Metadata.RuntimeLaunchID
	store.sessions[rec.ID] = stored
	if _, err := mgr.RestoreWithMode(t.Context(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if len(observer.mounts) != 2 || observer.mounts[0].Mode != domain.SessionModeTUI || observer.mounts[1].Mode != domain.SessionModeTUI {
		t.Fatalf("mount not resolved on spawn and restore: %+v", observer.mounts)
	}
}
