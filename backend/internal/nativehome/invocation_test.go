package nativehome

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestInvocationEnvironmentInheritsAndOverridesHome(t *testing.T) {
	inherited, override := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", inherited)
	for _, tc := range []struct {
		name    string
		overlay map[string]string
		want    string
	}{
		{"inherit", nil, inherited},
		{"override", map[string]string{"CLAUDE_CONFIG_DIR": override}, override},
		{"explicit-empty", map[string]string{"CLAUDE_CONFIG_DIR": ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := InvocationEnvironment(domain.HarnessClaudeCode, tc.overlay, "", "")
			if err != nil || env["CLAUDE_CONFIG_DIR"] != tc.want {
				t.Fatal(env["CLAUDE_CONFIG_DIR"], err)
			}
		})
	}
}
func TestInvocationOpenCodeHomeHasNoMigration(t *testing.T) {
	parent, ignored := t.TempDir(), t.TempDir()
	t.Setenv("XDG_DATA_HOME", parent)
	legacy := filepath.Join(parent, "opencode")
	if err := os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(legacy, "preserve")
	if err := os.WriteFile(marker, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	overlay := map[string]string{"XDG_DATA_HOME": ignored, "HOME": t.TempDir()}
	env, err := InvocationEnvironment(domain.HarnessOpenCodeV2, overlay, "", "")
	want := filepath.Join(parent, "opencode-v2-home")
	if err != nil || env["XDG_DATA_HOME"] != want {
		t.Fatal(env["XDG_DATA_HOME"], err)
	}
	if overlay["XDG_DATA_HOME"] != ignored {
		t.Fatal("mutated caller overlay")
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatal("guard prepared or migrated provider store", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "original" {
		t.Fatal(string(data), err)
	}
}
