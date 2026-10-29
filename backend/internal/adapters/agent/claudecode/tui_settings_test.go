package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestClaudeTUISettingsPreflightUsesVersionAndSessionLocalOptions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake executable")
	}
	for _, version := range []string{"2.1.204", "2.1.205", "unknown"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "claude")
			profile := filepath.Join(dir, "settings.json")
			before := []byte(`{"fastMode":false,"other":"unchanged"}`)
			if err := os.WriteFile(profile, before, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+version+" (Claude Code)'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			p := &Plugin{resolvedBinary: bin}
			preparer, ok := any(p).(interface {
				PrepareTUIAgentSettings(context.Context, ports.RestoreConfig, domain.AgentSettings, domain.AgentSettings) (domain.AgentSettingsProviderPlan, error)
			})
			if !ok {
				t.Fatal("Claude has no version-qualified session-local settings preflight")
			}
			source := domain.AgentSettings{Model: "native", Effort: "low", FastMode: domain.AgentFastOff}
			target := domain.AgentSettings{Model: "native", Effort: "high", FastMode: domain.AgentFastOn}
			plan, err := preparer.PrepareTUIAgentSettings(t.Context(), ports.RestoreConfig{Session: ports.SessionRef{WorkspacePath: dir, Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "native-thread"}}, Env: map[string]string{"CLAUDE_CONFIG_DIR": dir}}, source, target)
			if version == "2.1.205" {
				if err != nil || plan.After.Fast != "on" || plan.Before.Fast != "off" || plan.After.Effort != "high" {
					t.Fatalf("supported version preflight %+v %v", plan, err)
				}
			} else if err == nil {
				t.Fatalf("unqualified version accepted fast launch: %+v", plan)
			}
			after, readErr := os.ReadFile(profile)
			if readErr != nil || string(after) != string(before) {
				t.Fatalf("preflight rewrote profile: %q %v", after, readErr)
			}
		})
	}
}
