package sessionmanager

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/processalive"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
)

func TestAccountSignInAgainKeepsOrphanedChatProviderBlocking(t *testing.T) {
	const sessionID domain.SessionID = "orphaned-chat"
	const accountID = "managed-codex"
	root := t.TempDir()
	m, st, _, _ := newManager()
	m.dataDir, m.chat = root, &transitionChat{}
	st.sessions[sessionID] = domain.SessionRecord{ID: sessionID, Mode: domain.SessionModeChat, Harness: domain.HarnessCodex}
	m.store = &reviewerOwnerStore{fakeStore: st, owners: []domain.AgentAccountUsageOwner{
		{Kind: "session", SessionID: sessionID, Harness: domain.HarnessCodex, Blocking: true},
	}}
	profile := filepath.Join(root, "accounts")
	accounts := agentaccounts.New(&providerAccountStore{accounts: []domain.AgentAccount{
		{ID: accountID, Home: filepath.Join(profile, "codex", accountID), Provider: domain.AgentAccountProviderCodex},
	}}, []ports.AgentAccountProviderCLI{launchBoundaryProvider{&agentaccount.Codex{}}}, profile, nil, nil)
	accounts.SetLoginUsageOwners(m.AccountLoginUsageOwners)

	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	host := startLoginOwnerProcess(t)
	provider := startLoginOwnerProcess(t)
	dir := filepath.Join(root, "chat-hosts", string(sessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	descriptor := persistenthost.Descriptor{
		Version: persistenthost.ProtocolVersion, SessionID: string(sessionID), Address: "127.0.0.1:1",
		Token: "test-only", PID: host.Process.Pid, StartedAt: time.Now().UTC(), BootID: strings.TrimSpace(string(bootID)),
	}
	data, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host.lock"), []byte(strconv.Itoa(descriptor.PID)+" "+descriptor.BootID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host.spawn-"+strconv.Itoa(descriptor.PID)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := host.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = host.Wait()
	if persistenthost.ProbeHost(root, string(sessionID)) != persistenthost.HostAbsent || !processalive.Alive(provider.Process.Pid) {
		t.Fatal("expected the host to be gone and the provider to remain live")
	}
	_, err = accounts.StartSignInAgain(t.Context(), accountID)
	var inUse *ports.AgentAccountInUseError
	if !errors.As(err, &inUse) || len(inUse.Owners) != 1 || inUse.Owners[0].SessionID != sessionID || !inUse.Owners[0].Blocking {
		t.Fatalf("sign in again did not preserve the Chat owner: %v", err)
	}
}

func startLoginOwnerProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	return cmd
}
