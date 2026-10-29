package codexappserver

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestManagedChatRefusesMissingFileEvidenceBeforeSpawn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux managed profiles")
	}
	d := New(fixedCodexPlugin("fake-codex"), nil)
	d.persistent = false
	spawned := false
	d.spawn = func(context.Context, string, string, []string) (*process, error) {
		spawned = true
		return nil, errors.New("fake spawn")
	}
	_, err := d.connect(context.Background(), t.TempDir(), map[string]string{agentlaunch.ManagedCodexAccountEnv: "1", "CODEX_HOME": t.TempDir()}, "")
	if !errors.Is(err, ports.ErrAgentAccountCredentialStoreUnsupported) || spawned {
		t.Fatalf("missing file evidence reached provider: spawned=%v err=%v", spawned, err)
	}
}

func TestPersistentManagedAccountStartsWithFileStore(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("file-only managed profiles are Linux-only")
	}
	for _, prepared := range []bool{false, true} {
		d := New(fixedCodexPlugin("fake-codex"), nil)
		home := t.TempDir()
		if err := os.Chmod(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("fake-credential"), 0o600); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{agentlaunch.ManagedCodexAccountEnv: "1", "CODEX_HOME": home}
		var prepare func(context.Context) (map[string]string, error)
		if prepared {
			prepare = func(context.Context) (map[string]string, error) { return env, nil }
		}
		called := false
		d.connectHost = func(ctx context.Context, cfg persistenthost.Config) (*persistenthost.Transport, error) {
			called = true
			argv := cfg.Argv
			providerEnv := cfg.Env
			if cfg.Prepare != nil {
				p, err := cfg.Prepare(ctx)
				if err != nil {
					t.Fatal(err)
				}
				argv = p.Argv
				providerEnv = p.Env
			}
			for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
				for _, entry := range providerEnv {
					if strings.HasPrefix(entry, key+"=") && entry != key+"=" {
						t.Errorf("persistent managed Chat received %s", key)
					}
				}
			}
			if !strings.Contains(strings.Join(argv, " "), `-c cli_auth_credentials_store="file" app-server`) {
				t.Errorf("managed Chat follows keyring config: %q", argv)
			}
			return nil, errors.New("fake host: provider must not run")
		}
		for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
			t.Setenv(key, "fake-ambient-credential")
			env[key] = "fake-project-credential"
		}
		_, _, _ = d.connectSession(context.Background(), "session-1", "/fake/data", "/fake/workspace", env, prepare, "", false)
		if !called {
			t.Fatal("persistent host was not reached")
		}
	}
}

func TestDirectManagedAppServerUsesFileStoreAndDeviceDoesNotInheritIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux managed provider launcher")
	}
	t.Setenv(agentlaunch.ManagedCodexAccountEnv, "1")
	bin := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, managed := range []bool{false, true} {
		env := map[string]string{}
		if managed {
			env[agentlaunch.ManagedCodexAccountEnv] = "1"
		}
		p, err := spawnAppServer(context.Background(), bin, t.TempDir(), envSlice(env))
		if err != nil {
			t.Fatal(err)
		}
		output, err := io.ReadAll(p.stdout)
		_ = p.stop()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(output), `cli_auth_credentials_store="file"`); got != managed {
			t.Fatalf("managed=%v provider argv = %q", managed, output)
		}
	}
}
