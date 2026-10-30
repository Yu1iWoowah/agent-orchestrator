package claudecode

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/processenv"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

var minimumClaudeTUISettingsVersion = [3]int{2, 1, 205}

var claudeSettingsVersion = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:\s|$)`)

// PrepareTUIAgentSettings uses CLI flags/--settings only. They apply to this
// process, not the account's saved configuration. The documented session-local
// fast contract is available from 2.1.205; older/unparseable versions are
// unverified here, rather than inferred from a model name or --settings alone.
// https://code.claude.com/docs/en/fast-mode
func (p *Plugin) PrepareTUIAgentSettings(ctx context.Context, cfg ports.RestoreConfig, source, target domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	if source.Validate() != nil || target.Validate() != nil {
		return domain.AgentSettingsProviderPlan{}, ports.ErrAgentSettingsInvalid
	}
	if source.FastMode != domain.AgentFastDefault || target.FastMode != domain.AgentFastDefault || source.Effort != "" || target.Effort != "" {
		binary, err := p.claudeBinary(ctx)
		if err != nil {
			return domain.AgentSettingsProviderPlan{}, err
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := aoprocess.CommandContext(probeCtx, binary, "--version")
		cmd.Dir = cfg.Session.WorkspacePath
		cmd.Env = processenv.Merge(cfg.Env)
		raw, err := cmd.Output()
		match := claudeSettingsVersion.FindSubmatch(raw)
		if err != nil || len(match) != 4 {
			return domain.AgentSettingsProviderPlan{}, fmt.Errorf("%w: tui_settings_version_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
		var version [3]int
		for i := range version {
			value, parseErr := strconv.Atoi(string(match[i+1]))
			if parseErr != nil {
				return domain.AgentSettingsProviderPlan{}, fmt.Errorf("%w: tui_settings_version_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
			}
			version[i] = value
		}
		if slices.Compare(version[:], minimumClaudeTUISettingsVersion[:]) < 0 {
			return domain.AgentSettingsProviderPlan{}, fmt.Errorf("%w: tui_settings_version_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
	}
	resolve := func(value domain.AgentSettings) domain.AgentSettingsNativeValues {
		model := string(value.Model)
		if model == "" {
			model = "default"
		}
		// --effort is session-only; omission on a new process reads file defaults.
		// --settings omitted keys likewise retain file-based baseline values.
		return domain.AgentSettingsNativeValues{Model: model, Effort: string(value.Effort), Fast: string(value.FastMode)}
	}
	return domain.AgentSettingsProviderPlan{Protocol: "claude-native-resume", ProviderConversationID: cfg.Session.Metadata[ports.MetadataKeyAgentSessionID], Source: source, Target: target, Before: resolve(source), After: resolve(target), Permission: string(cfg.Permissions)}, nil
}
