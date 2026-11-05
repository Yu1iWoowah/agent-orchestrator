package nativehome

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/processenv"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// InvocationEnvironment applies inherited values and the launch bindings' home policy.
// It does not prepare profiles, migrate stores, or contact a provider.
func InvocationEnvironment(provider domain.AgentHarness, overlay map[string]string, dataDir string, hostID domain.SessionID) (map[string]string, error) {
	env := processenv.Map(overlay)
	switch provider {
	case domain.HarnessOpenCodeV2:
		home, err := OpenCodeV2DataHome()
		if err != nil {
			return nil, err
		}
		env["XDG_DATA_HOME"] = home
	case domain.HarnessKimi:
		if strings.TrimSpace(dataDir) != "" {
			env["KIMI_CODE_HOME"] = KimiHome(dataDir)
		}
	case domain.HarnessCommandCode:
		if home := CommandCodeHome(dataDir, hostID); home != "" {
			env["HOME"], env["USERPROFILE"] = home, home
		}
	}
	return env, nil
}

// KimiHome is the isolated root shared by Kimi's launch and native guard.
func KimiHome(dataDir string) string { return filepath.Join(dataDir, "kimi") }

// CommandCodeHome is the durable session home shared with the launch binding.
func CommandCodeHome(dataDir string, hostID domain.SessionID) string {
	dataDir, id := strings.TrimSpace(dataDir), strings.TrimSpace(string(hostID))
	if dataDir == "" || id == "" {
		return ""
	}
	return filepath.Join(dataDir, "harnesses", "command-code", id)
}

// OpenCodeV2DataHome computes the binding's daemon-derived root without migration.
func OpenCodeV2DataHome() (string, error) {
	parent := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if parent != "" && !filepath.IsAbs(parent) {
		return "", fmt.Errorf("opencode: XDG_DATA_HOME must be absolute, got %q", parent)
	}
	if filepath.Base(parent) == "opencode-v2-home" {
		return filepath.Clean(parent), nil
	}
	if parent == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("opencode: resolve absolute user data home: %w", err)
		}
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("opencode: user data home must be absolute, got %q", home)
		}
		parent = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(parent, "opencode-v2-home"), nil
}
