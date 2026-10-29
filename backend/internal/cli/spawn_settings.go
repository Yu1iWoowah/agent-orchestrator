package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func spawnSettingsPatch(cmd *cobra.Command, opts spawnOptions) (*domain.AgentSettingsPatch, error) {
	var patch domain.AgentSettingsPatch
	if cmd.Flags().Changed("model") && (strings.TrimSpace(opts.model) == "default" || strings.TrimSpace(opts.model) == "") {
		patch.Model = new(domain.AgentModelSelection(""))
	}
	if cmd.Flags().Changed("effort") && (strings.TrimSpace(opts.effort) == "default" || strings.TrimSpace(opts.effort) == "") {
		patch.Effort = new(domain.AgentEffortSelection(""))
	}
	if cmd.Flags().Changed("fast") {
		mode := domain.AgentFastMode(opts.fastMode)
		if mode != domain.AgentFastOn && mode != domain.AgentFastOff && mode != domain.AgentFastDefault {
			return nil, usageError{fmt.Errorf("--fast must be on, off, or default")}
		}
		patch.FastMode = &mode
	}
	if patch.Model == nil && patch.Effort == nil && patch.FastMode == nil {
		return nil, nil
	}
	return &patch, nil
}
