package codexappserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestManagedProbeNeutralizesCredentialEnv(t *testing.T) {
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, t.Name())
	}
	for _, id := range []string{"account-a", "account-b", "device"} {
		t.Run(id, func(t *testing.T) {
			called := false
			f := NewAccountFactory(fakePlugin{bin: "fake-codex"}, nil)
			f.spawn = func(_ context.Context, _ string, _ string, env []string, _ []string) (*process, error) {
				called = true
				for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
					for _, value := range env {
						if strings.HasPrefix(value, key+"=") && ((id != "device" && value != key+"=") || (id == "device" && value == key+"=")) {
							t.Errorf("managed account probe received ambient %s", key)
						}
					}
				}
				return nil, errors.New("fake-spawn-stop")
			}
			home := t.TempDir()
			if err := os.Chmod(home, 0o700); err != nil {
				t.Fatal(err)
			}
			_, err := f.Open(context.Background(), ports.CodexAccountContext{Home: home, Managed: id != "device"})
			if !called {
				t.Fatalf("fixture did not reach spawn: %v", err)
			}
		})
	}
}

func TestManagedModelProbeNeutralizesCredentialEnv(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("fake-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CODEX_HOME": home, agentlaunch.ManagedCodexAccountEnv: "1"}
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "fake-ambient-credential")
		env[key] = "fake-project-credential"
	}
	d := New(fakePlugin{bin: "fake-codex"}, nil)
	called := false
	d.spawn = func(_ context.Context, _, _ string, providerEnv []string) (*process, error) {
		called = true
		for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
			for _, entry := range providerEnv {
				if strings.HasPrefix(entry, key+"=") && entry != key+"=" {
					t.Errorf("managed model probe received %s", key)
				}
			}
		}
		return nil, errors.New("fake probe stop")
	}
	_, _ = d.DiscoverModels(context.Background(), t.TempDir(), env)
	if !called {
		t.Fatal("model fixture did not reach spawn")
	}
}

func TestCapabilityProbesDoNotReceiveCredentials(t *testing.T) {
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		t.Setenv(key, "fake-ambient-credential")
	}
	bin := filepath.Join(t.TempDir(), "fake-codex")
	script := "#!/bin/sh\nif [ -n \"$OPENAI_API_KEY$CODEX_API_KEY$CODEX_ACCESS_TOKEN\" ]; then exit 42; fi\nprintf 'codex-cli 0.165.0\\n'\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if version, err := installedCodexVersion(context.Background(), bin); err != nil || !strings.Contains(version, "0.165.0") {
		t.Fatalf("version probe received credentials: %v", err)
	}
	f := NewAccountFactory(fakePlugin{bin: bin}, nil)
	if got := f.detectCapabilities(context.Background(), bin); got.NativeLogin.State != domain.CodexCapabilitySupported {
		t.Fatal("schema/help probes received credentials or did not run")
	}
}
