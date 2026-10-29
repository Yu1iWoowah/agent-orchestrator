package agentaccounts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type isolatedOrphanStore struct {
	*memStore
	missing domain.AgentAccountProvider
}

func (s *isolatedOrphanStore) GetSessionAgentAccount(_ context.Context, _ domain.SessionID, p domain.AgentAccountProvider) (string, bool, error) {
	if p == s.missing {
		return "deleted-" + string(p), true, nil
	}
	return domain.DeviceAgentAccountID(p), true, nil
}
func TestInactiveOrphanEnvironmentCannotUseDeviceCredentials(t *testing.T) {
	for _, tc := range []struct {
		provider              domain.AgentAccountProvider
		active, orphanHarness domain.AgentHarness
		homeKey               string
		credentialKeys        []string
	}{
		{domain.AgentAccountProviderCodex, domain.HarnessClaudeCode, domain.HarnessCodex, "CODEX_HOME", []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"}},
		{domain.AgentAccountProviderClaude, domain.HarnessCodex, domain.HarnessClaudeCode, "CLAUDE_CONFIG_DIR", []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"}},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			root := filepath.Join(t.TempDir(), "accounts")
			device := t.TempDir()
			const marker = "fake-device-credential-marker"
			if e := os.WriteFile(filepath.Join(device, "credential-marker"), []byte(marker), 0600); e != nil {
				t.Fatal(e)
			}
			for _, k := range tc.credentialKeys {
				t.Setenv(k, marker)
			}
			store := &isolatedOrphanStore{memStore: &memStore{}, missing: tc.provider}
			s := New(store, []ports.AgentAccountProviderCLI{&agentaccount.Claude{}, &agentaccount.Codex{}}, root, nil, nil)
			env, e := s.SessionLaunchEnv(ctx, "old", tc.active)
			if e != nil {
				t.Fatalf("inactive orphan blocked active harness: %v", e)
			}
			home := filepath.Join(root, ".unavailable", string(tc.provider))
			if env[tc.homeKey] != home {
				t.Errorf("orphan home was not pinned to the unavailable namespace: %q", env[tc.homeKey])
			}
			if tc.provider == domain.AgentAccountProviderCodex {
				if env[agentlaunch.ManagedCodexAccountEnv] != "1" {
					t.Error("orphan Codex lost managed marker")
				}
				if e := agentlaunch.ValidateManagedCodexEnv(env); !errors.Is(e, ports.ErrAgentAccountCredentialStoreUnsupported) {
					t.Errorf("orphan Codex passed native launch validation: %v", e)
				}
			} else if env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != home {
				t.Error("orphan Claude left secure storage on device")
			}
			var vars strings.Builder
			for _, k := range tc.credentialKeys {
				if v, ok := env[k]; !ok || v != "" {
					t.Errorf("ambient credential override %s not neutralized", k)
				}
				fmt.Fprintf(&vars, "${%s}", k)
			}
			// A fake provider models both credential-env precedence and CLI's normal
			// fallback to the device home when its config-home variable is absent.
			script := fmt.Sprintf(`if [ -n "%s" ]; then printf 'ambient-credential'; exit 0; fi
home="${%s:-$FAKE_DEVICE_HOME}"
if [ -f "$home/credential-marker" ]; then cat "$home/credential-marker"; exit 0; fi
printf 'unavailable'; exit 23`, vars.String(), tc.homeKey)
			for _, clearAmbient := range []bool{false, true} {
				cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
				cmd.Env = append(os.Environ(), "FAKE_DEVICE_HOME="+device)
				for k, v := range env {
					cmd.Env = append(cmd.Env, k+"="+v)
				}
				if clearAmbient {
					for _, k := range tc.credentialKeys {
						cmd.Env = append(cmd.Env, k+"=")
					}
				}
				out, e := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(e, &exit) || exit.ExitCode() != 23 || string(out) != "unavailable" {
					t.Errorf("fake provider fell back to credentials (clearAmbient=%t): exit=%v output=%q", clearAmbient, e, out)
				}
			}
			if _, e := os.Lstat(home); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("environment resolution prepared an unavailable profile")
			}
			again, e := s.SessionLaunchEnv(ctx, "old", tc.active)
			if e != nil || again[tc.homeKey] != home {
				t.Fatal("orphan pin was not stable")
			}
			if _, e := s.SessionLaunchEnv(ctx, "old", tc.orphanHarness); !errors.Is(e, ports.ErrAgentAccountUnknown) {
				t.Fatalf("active orphan no longer refused: %v", e)
			}
			if id, e := s.SessionAccountID(ctx, "old", tc.orphanHarness); e != nil || id != "deleted-"+string(tc.provider) {
				t.Fatal("environment isolation rewrote historical binding")
			}
			data, e := os.ReadFile(filepath.Join(device, "credential-marker"))
			if e != nil || string(data) != marker {
				t.Fatal("fake device state changed")
			}
		})
	}
}

func TestOrphanEnvironmentRejectsOccupiedUnavailableNamespace(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "accounts")
			if e := os.Mkdir(root, 0700); e != nil {
				t.Fatal(e)
			}
			reserved := filepath.Join(root, ".unavailable")
			switch kind {
			case "directory":
				home := filepath.Join(reserved, "codex")
				if e := os.MkdirAll(home, 0700); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(filepath.Join(home, "auth.json"), []byte("fake-unavailable-credential"), 0600); e != nil {
					t.Fatal(e)
				}
			case "file":
				if e := os.WriteFile(reserved, []byte("fake-owned-file"), 0600); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				if e := os.Symlink(t.TempDir(), reserved); e != nil {
					t.Fatal(e)
				}
			}
			s := New(&isolatedOrphanStore{memStore: &memStore{}, missing: domain.AgentAccountProviderCodex}, []ports.AgentAccountProviderCLI{&agentaccount.Claude{}, &agentaccount.Codex{}}, root, nil, nil)
			env, e := s.SessionLaunchEnv(context.Background(), "old", domain.HarnessClaudeCode)
			if !errors.Is(e, ports.ErrAgentAccountStorageUnsafe) {
				t.Errorf("occupied unavailable namespace accepted: %v", e)
			}
			if env["CODEX_HOME"] != filepath.Join(reserved, "codex") || env[agentlaunch.ManagedCodexAccountEnv] != "1" {
				t.Error("error discarded the pinned environment")
			}
		})
	}
}
