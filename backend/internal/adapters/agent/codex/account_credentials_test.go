package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestManagedAccountLaunchUsesFileCredentialsDespiteSharedKeyringConfig(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("new file-only managed profiles are Linux-only")
	}
	device := t.TempDir()
	home := filepath.Join(t.TempDir(), "account")
	config := []byte("cli_auth_credentials_store = \"keyring\"\n")
	if err := os.WriteFile(filepath.Join(device, "config.toml"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	account := &agentaccount.Codex{DeviceHome: device}
	if err := account.Prepare(home); err != nil {
		t.Fatal(err)
	}
	p := &Plugin{resolvedBinary: "codex"}
	argv, err := p.GetLaunchCommand(context.Background(), ports.LaunchConfig{Env: account.LaunchEnv(home)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, " "), `-c cli_auth_credentials_store="file"`) {
		t.Fatalf("managed launch still follows shared keyring config: %q", argv)
	}
	argv, ok, err := p.GetRestoreCommand(context.Background(), ports.RestoreConfig{
		Env:     account.LaunchEnv(home),
		Session: ports.SessionRef{Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "fake-thread"}},
	})
	if err != nil || !ok || !strings.Contains(strings.Join(argv, " "), `-c cli_auth_credentials_store="file"`) {
		t.Fatalf("managed resume still follows shared keyring config: %q, %v", argv, err)
	}
	data, err := os.ReadFile(filepath.Join(device, "config.toml"))
	if err != nil || string(data) != string(config) {
		t.Fatalf("device config changed: %q, %v", data, err)
	}
}

func TestDeviceAccountCommandsKeepConfiguredCredentialStore(t *testing.T) {
	p := &Plugin{resolvedBinary: "codex"}
	account := &agentaccount.Codex{DeviceHome: "/fake-device"}
	for _, home := range []string{"", "/fake-device"} {
		argv, err := p.GetLaunchCommand(context.Background(), ports.LaunchConfig{Env: account.LaunchEnv(home)})
		if err != nil || strings.Contains(strings.Join(argv, " "), "cli_auth_credentials_store") {
			t.Fatalf("device launch forced an account store: %q, %v", argv, err)
		}
		argv, ok, err := p.GetRestoreCommand(context.Background(), ports.RestoreConfig{
			Env:     account.LaunchEnv(home),
			Session: ports.SessionRef{Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "fake-thread"}},
		})
		if err != nil || !ok || strings.Contains(strings.Join(argv, " "), "cli_auth_credentials_store") {
			t.Fatalf("device resume forced an account store: %q, %v", argv, err)
		}
	}
}
