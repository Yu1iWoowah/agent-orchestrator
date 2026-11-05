package nativehome

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestResolveProviderInvocation(t *testing.T) {
	for _, tc := range []struct {
		provider      domain.AgentHarness
		key, relative string
	}{
		{domain.HarnessCodex, "CODEX_HOME", ".codex"},
		{domain.HarnessClaudeCode, "CLAUDE_CONFIG_DIR", ".claude"},
		{domain.HarnessOpenCode, "OPENCODE_DATA_DIR", ".local/share/opencode"},
		{domain.HarnessOpenCodeV2, "XDG_DATA_HOME", ".local/share/opencode-v2-home/opencode"},
		{domain.HarnessOMP, "PI_CODING_AGENT_DIR", ".omp/agent"},
		{domain.HarnessPi, "PI_CODING_AGENT_DIR", ".pi/agent"},
		{domain.HarnessCursor, "", ".cursor"},
		{domain.HarnessDroid, "", ".factory"},
		{domain.HarnessKimi, "KIMI_CODE_HOME", ".kimi-code"},
		{domain.HarnessKimchi, "", ".config/kimchi/harness"},
		{domain.HarnessQwen, "QWEN_HOME", ".qwen"},
		{domain.HarnessAgy, "GEMINI_HOME", ".gemini"},
		{domain.HarnessCommandCode, "", ".commandcode"},
		{domain.HarnessDeepSeek, "DSH_HOME", ".dsh"},
		{domain.HarnessCopilot, "COPILOT_HOME", ".copilot"},
		{domain.HarnessUnreal, "", "agent-runtime/unreal-agent/sessions"},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			home := t.TempDir()
			env := map[string]string{"HOME": home}
			want := filepath.Join(home, filepath.FromSlash(tc.relative))
			if err := os.MkdirAll(want, 0700); err != nil {
				t.Fatal(err)
			}
			got, err := Resolve(t.Context(), tc.provider, env, home, home)
			if err != nil || got != want {
				t.Fatalf("default root=%q, %v; want %q", got, err, want)
			}
			if tc.key == "" {
				return
			}
			target := t.TempDir()
			link := filepath.Join(home, "override")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			env[tc.key] = link
			want = target
			if tc.provider == domain.HarnessOpenCodeV2 {
				want = filepath.Join(target, "opencode-v2-home", "opencode")
				if err := os.MkdirAll(want, 0700); err != nil {
					t.Fatal(err)
				}
			}
			got, err = Resolve(t.Context(), tc.provider, env, home, home)
			if err != nil || got != want {
				t.Fatalf("invocation root=%q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestResolveOpenCodeHonorsDataRoot(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	want := filepath.Join(data, "opencode")
	if err := os.Mkdir(want, 0700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "XDG_DATA_HOME": data, "CLAUDE_CONFIG_DIR": t.TempDir()}
	got, err := Resolve(t.Context(), domain.HarnessOpenCode, env, home, "")
	if err != nil || got != want {
		t.Fatalf("data root=%q, %v", got, err)
	}
	env["OPENCODE_DATA_DIR"] = "missing"
	if _, err := Resolve(t.Context(), domain.HarnessOpenCode, env, home, ""); err == nil {
		t.Fatal("missing own root fell back to another provider")
	}
	env["OPENCODE_DATA_DIR"] = filepath.Join(home, "file")
	if err := os.WriteFile(env["OPENCODE_DATA_DIR"], []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(t.Context(), domain.HarnessOpenCode, env, home, ""); err == nil {
		t.Fatal("file accepted as native home")
	}
}

func TestResolveUsesChildHomeNotDaemonOverrides(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	home := t.TempDir()
	want := filepath.Join(home, ".codex")
	if err := os.Mkdir(want, 0700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "USERPROFILE"} {
		got, err := Resolve(t.Context(), domain.HarnessCodex, map[string]string{key: home}, home, "")
		if err != nil || got != want {
			t.Fatalf("%s root=%q, %v", key, got, err)
		}
	}
	if _, err := Resolve(t.Context(), domain.HarnessFake, map[string]string{"HOME": home}, home, ""); err == nil {
		t.Fatal("unknown provider inherited another root")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Resolve(ctx, domain.HarnessCodex, nil, home, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestResolveExplicitRootDoesNotRequireHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	root := t.TempDir()
	got, err := Resolve(t.Context(), domain.HarnessCodex, map[string]string{"CODEX_HOME": root}, root, "")
	if err != nil || got != root {
		t.Fatalf("explicit root=%q, %v", got, err)
	}
	want := filepath.Join(root, "opencode")
	if err := os.Mkdir(want, 0700); err != nil {
		t.Fatal(err)
	}
	got, err = Resolve(t.Context(), domain.HarnessOpenCode, map[string]string{"XDG_DATA_HOME": root}, root, "")
	if err != nil || got != want {
		t.Fatalf("explicit data root=%q, %v", got, err)
	}
}
