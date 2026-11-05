// Package nativehome resolves provider-owned state from an invocation's environment.
package nativehome

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Resolve canonicalizes the state root used by a launched provider, never another provider's root.
func Resolve(ctx context.Context, provider domain.AgentHarness, env map[string]string, workdir, dataDir string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	rule, known := map[domain.AgentHarness]struct{ key, fallback string }{
		domain.HarnessCodex:       {"CODEX_HOME", ".codex"},
		domain.HarnessClaudeCode:  {"CLAUDE_CONFIG_DIR", ".claude"},
		domain.HarnessOpenCode:    {"OPENCODE_DATA_DIR", ".local/share/opencode"},
		domain.HarnessOpenCodeV2:  {"", ".local/share/opencode-v2-home/opencode"},
		domain.HarnessOMP:         {"PI_CODING_AGENT_DIR", ".omp/agent"},
		domain.HarnessPi:          {"PI_CODING_AGENT_DIR", ".pi/agent"},
		domain.HarnessCursor:      {"", ".cursor"},
		domain.HarnessDroid:       {"", ".factory"},
		domain.HarnessKimi:        {"KIMI_CODE_HOME", ".kimi-code"},
		domain.HarnessKimchi:      {"", ".config/kimchi/harness"},
		domain.HarnessQwen:        {"QWEN_HOME", ".qwen"},
		domain.HarnessAgy:         {"GEMINI_HOME", ".gemini"},
		domain.HarnessCommandCode: {"", ".commandcode"},
		domain.HarnessDeepSeek:    {"DSH_HOME", ".dsh"},
		domain.HarnessCopilot:     {"COPILOT_HOME", ".copilot"},
		domain.HarnessUnreal:      {"", ""},
	}[provider]
	if !known {
		return "", fmt.Errorf("native state root is unknown for %s", provider)
	}
	invocationHome := func() (string, error) {
		if home := strings.TrimSpace(env["HOME"]); home != "" {
			return home, nil
		}
		if home := strings.TrimSpace(env["USERPROFILE"]); home != "" {
			return home, nil
		}
		return os.UserHomeDir()
	}
	root := strings.TrimSpace(env[rule.key])
	switch provider {
	case domain.HarnessUnreal:
		root = filepath.Join(dataDir, "agent-runtime", string(provider), "sessions")
	case domain.HarnessOpenCode, domain.HarnessOpenCodeV2:
		if provider == domain.HarnessOpenCodeV2 || root == "" {
			dataHome := strings.TrimSpace(env["XDG_DATA_HOME"])
			if dataHome == "" {
				home, err := invocationHome()
				if err != nil {
					return "", err
				}
				dataHome = filepath.Join(home, ".local", "share")
			}
			if provider == domain.HarnessOpenCodeV2 && filepath.Base(dataHome) != "opencode-v2-home" {
				dataHome = filepath.Join(dataHome, "opencode-v2-home")
			}
			root = filepath.Join(dataHome, "opencode")
		}
	default:
		if root == "" {
			home, err := invocationHome()
			if err != nil {
				return "", err
			}
			root = filepath.Join(home, filepath.FromSlash(rule.fallback))
		}
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(workdir, root)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("native state root for %s is not a directory", provider)
	}
	return root, nil
}
