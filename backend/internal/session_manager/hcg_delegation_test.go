package sessionmanager

import (
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type delegationMountObserver struct{ notices []ports.HCGReportingLaunch }

func (o *delegationMountObserver) ReportingLaunchReady(n ports.HCGReportingLaunch) {
	o.notices = append(o.notices, n)
}
func (*delegationMountObserver) DelegationMountRequested(ports.HCGDelegationMount) bool { return true }

func TestHCGDelegationChatMountFreshAndRestore(t *testing.T) {
	launcher := &recordingLauncher{}
	mgr, store, _ := newChatManager(launcher)
	mgr.dataDir = t.TempDir()
	executable := filepath.Join(t.TempDir(), "ao")
	mgr.executable = func() (string, error) { return executable, nil }
	mgr.hcgReporting = &delegationMountObserver{}
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
		hcg := start.MCPServers[1]
		if len(hcg.Args) != 3 || hcg.Args[0] != "usage" || hcg.Args[1] != "delegation" || hcg.Args[2] != "mcp" || hcg.Env[EnvSessionID] != string(rec.ID) {
			t.Fatalf("fixed HCG config=%+v", hcg)
		}
		for key := range hcg.Env {
			if key != EnvSessionID && key != EnvDataDir && key != EnvRunFile {
				t.Fatalf("unexpected forwarded env name: %s", key)
			}
		}
	}
	if got := mgr.chatMCPServers(domain.HarnessCodex, domain.SessionKind("reviewer"), "review", "p", map[string]string{}); len(got) != 1 || got[0].Name != "ao" {
		t.Fatal("reviewer received delegation")
	}
}

func TestHCGDelegationTUIMountSuccessAndRestore(t *testing.T) {
	mgr, store, _, ws := newManager()
	ws.path = t.TempDir()
	mgr.dataDir = t.TempDir()
	observer := &delegationMountObserver{}
	mgr.hcgReporting = observer
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
	if len(observer.notices) != 2 || !observer.notices[0].DelegationConfigured || !observer.notices[1].DelegationConfigured {
		t.Fatalf("actual mount not recorded: %+v", observer.notices)
	}
}
