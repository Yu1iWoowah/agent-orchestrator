package agentaccount

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fileSnapshot struct {
	Mode fs.FileMode
	Data string
}

func snapshot(t *testing.T, root string) map[string]fileSnapshot {
	t.Helper()
	out := map[string]fileSnapshot{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data := []byte(nil)
		if info.Mode().IsRegular() {
			data, err = os.ReadFile(path)
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = fileSnapshot{info.Mode(), string(data)}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestContainedProfilesDoNotTouchDevice(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("new managed profiles are Linux-only")
	}
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			root, device := filepath.Join(t.TempDir(), "accounts"), t.TempDir()
			write(t, filepath.Join(device, "settings.json"), `{"env":{"SECRET":"fake-device-marker"}}`)
			write(t, filepath.Join(device, "config.toml"), `credential_marker="fake-device-marker"`)
			write(t, filepath.Join(device, "unknown-new-file"), "fake-device-marker")
			before := snapshot(t, device)
			var prepare func(string) error
			if provider == "claude" {
				prepare = (&Claude{DeviceHome: device, DeviceConfig: filepath.Join(device, ".claude.json")}).Prepare
			} else {
				prepare = (&Codex{DeviceHome: device}).Prepare
			}
			for _, id := range []string{"a", "b"} {
				home := filepath.Join(root, provider, id)
				if err := prepare(home); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(home, "auth.json"), "fake-account-"+id)
				write(t, filepath.Join(home, "models_cache.json"), id)
				write(t, filepath.Join(home, "ipc", "socket-marker"), id)
				if err := prepare(home); err != nil {
					t.Fatal(err)
				}
				entries, _ := os.ReadDir(home)
				for _, entry := range entries {
					path := filepath.Join(home, entry.Name())
					if entry.Type()&os.ModeSymlink != 0 {
						target, err := filepath.EvalSymlinks(path)
						if err != nil || !strings.HasPrefix(target, filepath.Join(root, "history", provider)+string(os.PathSeparator)) {
							t.Errorf("history escaped AO root: %s", entry.Name())
						}
					}
				}
				for _, name := range []string{"auth.json", "models_cache.json", "ipc"} {
					info, err := os.Lstat(filepath.Join(home, name))
					if err != nil || info.Mode()&os.ModeSymlink != 0 {
						t.Errorf("private entry shared: %s", name)
					}
				}
				for _, name := range []string{"unknown-new-file", "settings.json", "config.toml"} {
					data, _ := os.ReadFile(filepath.Join(home, name))
					if strings.Contains(string(data), "fake-device-marker") {
						t.Errorf("device marker imported: %s", name)
					}
				}
			}
			a, b := filepath.Join(root, provider, "a"), filepath.Join(root, provider, "b")
			write(t, filepath.Join(a, "sessions", "resume.jsonl"), "fake-conversation-history")
			if data, err := os.ReadFile(filepath.Join(b, "sessions", "resume.jsonl")); err != nil || string(data) != "fake-conversation-history" {
				t.Fatalf("AO-owned history cannot resume across profiles: %v", err)
			}
			write(t, filepath.Join(a, "unknown-new-file"), "fake-account-a-private")
			if err := prepare(a); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(b, "unknown-new-file")); !os.IsNotExist(err) {
				t.Fatal("unknown account entry crossed profiles")
			}
			if !reflect.DeepEqual(before, snapshot(t, device)) {
				t.Error("external device contents or modes changed")
			}
		})
	}
}

func TestUnknownFilesNeverBecomeShared(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("new managed profiles are Linux-only")
	}
	root, device := filepath.Join(t.TempDir(), "accounts"), t.TempDir()
	write(t, filepath.Join(device, "new-provider-secret"), "fake-secret")
	home := filepath.Join(root, "codex", "a")
	if err := (&Codex{DeviceHome: device}).Prepare(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, "new-provider-secret")); !os.IsNotExist(err) {
		t.Fatal("unknown device entry was shared")
	}
}

func TestContainedSettingsKeepProviderModes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("new managed profiles are Linux-only")
	}
	for _, provider := range []string{"claude", "codex"} {
		for _, mode := range []fs.FileMode{0o644, 0o664} {
			t.Run(provider+"/"+mode.String(), func(t *testing.T) {
				home := filepath.Join(t.TempDir(), "accounts", provider, "a")
				prepare := (&Codex{}).Prepare
				if provider == "claude" {
					prepare = (&Claude{}).Prepare
				}
				if err := prepare(home); err != nil {
					t.Fatal(err)
				}
				names := []string{"models_cache.json", "settings.json", "settings.local.json", "config.toml", ".claude.json"}
				for _, name := range names {
					path := filepath.Join(home, name)
					write(t, path, "fake-provider-settings")
					if err := os.Chmod(path, mode); err != nil {
						t.Fatal(err)
					}
				}
				if err := prepare(home); err != nil {
					t.Fatalf("provider settings rejected: %v", err)
				}
				for _, name := range names {
					info, err := os.Stat(filepath.Join(home, name))
					if err != nil || info.Mode().Perm() != mode {
						t.Fatalf("provider settings mode changed: %s: %v", name, err)
					}
					if data, err := os.ReadFile(filepath.Join(home, name)); err != nil || string(data) != "fake-provider-settings" {
						t.Fatalf("provider settings contents changed: %s: %v", name, err)
					}
				}
			})
		}
	}
}

func TestContainedCredentialAndIPCRequirePrivateModes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("new managed profiles are Linux-only")
	}
	for _, name := range []string{"auth.json", ".credentials.json", "ipc", "app-server-control", "app-server-daemon", "app-server-new-endpoint"} {
		t.Run(name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "accounts", "codex", "a")
			c := &Codex{}
			if err := c.Prepare(home); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, name)
			mode := fs.FileMode(0o644)
			if name == "ipc" || name == "app-server-daemon" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				mode = 0o755
			} else {
				write(t, path, "fake-private-data")
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if err := c.Prepare(home); !errors.Is(err, ports.ErrAgentAccountStorageUnsafe) {
				t.Fatalf("public credential/IPC accepted: %v", err)
			}
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
				t.Fatalf("unsafe credential/IPC was chmodded: %v", err)
			}
		})
	}
}

func TestContainedHistoryAndPrivatePathsRejectReplacement(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("new managed profiles are Linux-only")
	}
	for _, kind := range []string{"history-root-link", "history-target-link", "history-file-hardlink", "profile-link", "credential-link", "config-hardlink", "unknown-link"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "accounts")
			home := filepath.Join(root, "codex", "a")
			c := &Codex{DeviceHome: filepath.Join(t.TempDir(), "unused-device")}
			if err := c.Prepare(home); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			write(t, filepath.Join(outside, "marker"), "untouched")
			before := snapshot(t, outside)
			var err error
			switch kind {
			case "history-root-link":
				path := filepath.Join(root, "history", "codex")
				if err = os.RemoveAll(path); err == nil {
					err = os.Symlink(outside, path)
				}
			case "history-target-link":
				path := filepath.Join(root, "history", "codex", "sessions")
				if err = os.Remove(path); err == nil {
					err = os.Symlink(outside, path)
				}
			case "history-file-hardlink":
				path := filepath.Join(root, "history", "codex", "history.jsonl")
				if err = os.Remove(path); err == nil {
					err = os.Link(filepath.Join(outside, "marker"), path)
				}
			case "profile-link":
				if err = os.RemoveAll(home); err == nil {
					err = os.Symlink(outside, home)
				}
			case "credential-link":
				err = os.Symlink(filepath.Join(outside, "marker"), filepath.Join(home, "auth.json"))
			case "config-hardlink":
				err = os.Link(filepath.Join(outside, "marker"), filepath.Join(home, "config.toml"))
			case "unknown-link":
				err = os.Symlink(outside, filepath.Join(home, "unknown"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Prepare(home); !errors.Is(err, ports.ErrAgentAccountStorageUnsafe) {
				t.Fatalf("replacement accepted: %v", err)
			}
			if !reflect.DeepEqual(before, snapshot(t, outside)) {
				t.Fatal("external target changed")
			}
		})
	}
}
