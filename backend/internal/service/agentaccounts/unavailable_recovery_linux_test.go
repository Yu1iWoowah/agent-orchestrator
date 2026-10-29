package agentaccounts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestEmptyUnavailableNamespaceRecoversWithoutDeviceFallback(t *testing.T) {
	for _, children := range [][]string{nil, {"claude"}, {"codex", "claude"}} {
		t.Run(strings.Join(children, "+")+"empty", func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "accounts")
			reserved := filepath.Join(root, ".unavailable")
			if e := os.MkdirAll(reserved, 0700); e != nil {
				t.Fatal(e)
			}
			for _, name := range children {
				if e := os.Mkdir(filepath.Join(reserved, name), 0755); e != nil {
					t.Fatal(e)
				}
			}
			var logs bytes.Buffer
			s := New(&isolatedOrphanStore{memStore: &memStore{}, missing: domain.AgentAccountProviderCodex}, []ports.AgentAccountProviderCLI{&agentaccount.Claude{}, &agentaccount.Codex{}}, root, nil, slog.New(slog.NewTextHandler(&logs, nil)))
			env, e := s.SessionLaunchEnv(context.Background(), "old", domain.HarnessClaudeCode)
			if e != nil {
				t.Fatalf("empty reserved provider folders permanently blocked launch: %v", e)
			}
			if env["CODEX_HOME"] != filepath.Join(reserved, "codex") || env[agentlaunch.ManagedCodexAccountEnv] != "1" {
				t.Fatal("recovery dropped unavailable home pin")
			}
			if _, e := os.Lstat(reserved); !os.IsNotExist(e) {
				t.Fatal("empty reserved namespace was not removed")
			}
			if e := agentlaunch.ValidateManagedCodexEnv(env); e == nil {
				t.Fatal("recovery enabled orphan Codex")
			}
			if !strings.Contains(logs.String(), "account_unavailable_home_recovered") {
				t.Fatal("automatic recovery was not diagnosed")
			}
			if strings.Contains(logs.String(), root) {
				t.Fatal("recovery log exposed the account root")
			}
			if _, e := s.SessionLaunchEnv(context.Background(), "old", domain.HarnessClaudeCode); e != nil {
				t.Fatal(e)
			}
			if strings.Count(logs.String(), "account_unavailable_home_recovered") != 1 {
				t.Fatal("empty recovery was not idempotent")
			}
		})
	}
}
func TestUnsafeUnavailableNamespacePreservesDataAndExplainsRecovery(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "unknown-empty-directory", "credentials", "settings", "mixed", "provider-symlink", "nested-symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "accounts")
			reserved := filepath.Join(root, ".unavailable")
			target := t.TempDir()
			const marker = "fake-unavailable-sensitive-marker"
			if e := os.Mkdir(root, 0700); e != nil {
				t.Fatal(e)
			}
			targetFile := filepath.Join(target, "keep")
			if e := os.WriteFile(targetFile, []byte(marker), 0600); e != nil {
				t.Fatal(e)
			}
			var protected string
			switch kind {
			case "file":
				protected = reserved
				if e := os.WriteFile(protected, []byte(marker), 0600); e != nil {
					t.Fatal(e)
				}
			case "symlink":
				if e := os.Symlink(target, reserved); e != nil {
					t.Fatal(e)
				}
			default:
				if e := os.Mkdir(reserved, 0700); e != nil {
					t.Fatal(e)
				}
				if kind == "unknown-empty-directory" {
					if e := os.Mkdir(filepath.Join(reserved, "user-folder"), 0700); e != nil {
						t.Fatal(e)
					}
					break
				}
				if kind == "provider-symlink" {
					if e := os.Symlink(target, filepath.Join(reserved, "claude")); e != nil {
						t.Fatal(e)
					}
					break
				}
				home := filepath.Join(reserved, "claude")
				if e := os.Mkdir(home, 0700); e != nil {
					t.Fatal(e)
				}
				if kind == "nested-symlink" {
					if e := os.Symlink(target, filepath.Join(home, "link")); e != nil {
						t.Fatal(e)
					}
					break
				}
				name := ".credentials.json"
				if kind == "settings" {
					name = "settings.json"
				}
				protected = filepath.Join(home, name)
				if e := os.WriteFile(protected, []byte(marker), 0600); e != nil {
					t.Fatal(e)
				}
				if kind == "mixed" {
					if e := os.Mkdir(filepath.Join(reserved, "codex"), 0700); e != nil {
						t.Fatal(e)
					}
				}
			}
			var logs bytes.Buffer
			s := New(&isolatedOrphanStore{memStore: &memStore{}, missing: domain.AgentAccountProviderCodex}, []ports.AgentAccountProviderCLI{&agentaccount.Claude{}, &agentaccount.Codex{}}, root, nil, slog.New(slog.NewTextHandler(&logs, nil)))
			env, e := s.SessionLaunchEnv(context.Background(), "old", domain.HarnessClaudeCode)
			if e == nil || !strings.Contains(e.Error(), "account_unavailable_home_occupied") {
				t.Errorf("missing dedicated unavailable diagnostic: %v", e)
			}
			if env["CODEX_HOME"] != filepath.Join(reserved, "codex") || env[agentlaunch.ManagedCodexAccountEnv] != "1" {
				t.Error("blocked recovery discarded fail-closed env")
			}
			if _, e := os.Lstat(reserved); e != nil {
				t.Fatal("unsafe namespace was deleted")
			}
			if protected != "" {
				if b, e := os.ReadFile(protected); e != nil || string(b) != marker {
					t.Fatal("user data or credentials changed")
				}
			}
			if b, e := os.ReadFile(targetFile); e != nil || string(b) != marker {
				t.Fatal("cleanup followed a link into device data")
			}
			if kind == "mixed" {
				if _, e := os.Stat(filepath.Join(reserved, "codex")); e != nil {
					t.Fatal("preflight failure partially removed namespace")
				}
			}
			if !strings.Contains(logs.String(), "account_unavailable_home_occupied") || !strings.Contains(logs.String(), "Stop related child tools") {
				t.Error("log did not explain occupied namespace and recovery action")
			}
			if strings.Contains(logs.String(), marker) || strings.Contains(logs.String(), root) {
				t.Error("diagnostic leaked contents or absolute paths")
			}
		})
	}
}

func TestConcurrentUnavailableRecoveryKeepsIsolation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	if e := os.MkdirAll(filepath.Join(root, ".unavailable", "codex"), 0700); e != nil {
		t.Fatal(e)
	}
	s := New(&isolatedOrphanStore{memStore: &memStore{}, missing: domain.AgentAccountProviderCodex}, []ports.AgentAccountProviderCLI{&agentaccount.Claude{}, &agentaccount.Codex{}}, root, nil, nil)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			env, err := s.SessionLaunchEnv(context.Background(), "old", domain.HarnessClaudeCode)
			if err == nil && (env["CODEX_HOME"] != filepath.Join(root, ".unavailable", "codex") || env[agentlaunch.ManagedCodexAccountEnv] != "1") {
				err = fmt.Errorf("concurrent recovery lost isolation")
			}
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, ".unavailable")); !os.IsNotExist(err) {
		t.Fatal("empty namespace remains")
	}
}

func TestUnavailableRecoveryRejectsLinkedOrNonprivateRoot(t *testing.T) {
	for _, linked := range []bool{true, false} {
		t.Run(fmt.Sprint(linked), func(t *testing.T) {
			base := t.TempDir()
			actualRoot := filepath.Join(base, "real")
			reserved := filepath.Join(actualRoot, ".unavailable", "codex")
			if err := os.MkdirAll(reserved, 0700); err != nil {
				t.Fatal(err)
			}
			root := actualRoot
			if linked {
				root = filepath.Join(base, "link")
				if err := os.Symlink(actualRoot, root); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(actualRoot, 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := recoverUnavailableNamespace(root); !errors.Is(err, ports.ErrAgentAccountStorageUnsafe) {
				t.Fatalf("unsafe root accepted: %v", err)
			}
			if _, err := os.Stat(reserved); err != nil {
				t.Fatal("unsafe root contents changed")
			}
		})
	}
}
