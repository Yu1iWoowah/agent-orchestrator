package domain

import (
	"fmt"
	"slices"
)

// AgentSettingsPolicy authorizes managed orchestrator changes only. Local
// unverified manual calls bypass this policy, never runtime validation or CAS.
type AgentSettingsPolicy struct {
	Enabled   bool                                    `json:"enabled"`
	Harnesses map[AgentHarness]AgentSettingsAllowance `json:"harnesses"`
}

// AgentSettingsAllowance is an explicit set; empty lists allow no selections.
// An empty model/effort entry explicitly allows provider default. AllowFast
// permits requested on; default retains provider semantics, not guaranteed off.
type AgentSettingsAllowance struct {
	Models    []AgentModelSelection  `json:"models"`
	Efforts   []AgentEffortSelection `json:"efforts"`
	AllowFast bool                   `json:"allowFast"`
}

// Validate rejects malformed typed policy without inferring price or features.
func (p AgentSettingsPolicy) Validate() error {
	for harness, a := range p.Harnesses {
		if !harness.IsKnown() {
			return fmt.Errorf("agentSettingsPolicy: unknown harness %q", harness)
		}
		if a.AllowFast && !harness.SupportsFastMode() {
			return fmt.Errorf("agentSettingsPolicy: fast is unsupported for %s", harness)
		}
		if len(a.Models) > 128 || len(a.Efforts) > 128 {
			return fmt.Errorf("agentSettingsPolicy: allowlist is too large")
		}
		models := make(map[AgentModelSelection]bool, len(a.Models))
		for _, model := range a.Models {
			if err := (AgentSettings{Model: model, FastMode: AgentFastDefault}).Validate(); err != nil {
				return fmt.Errorf("agentSettingsPolicy: %w", err)
			}
			if models[model] {
				return fmt.Errorf("agentSettingsPolicy: duplicate model")
			}
			models[model] = true
		}
		efforts := make(map[AgentEffortSelection]bool, len(a.Efforts))
		for _, effort := range a.Efforts {
			if err := (AgentSettings{Effort: effort, FastMode: AgentFastDefault}).Validate(); err != nil {
				return fmt.Errorf("agentSettingsPolicy: %w", err)
			}
			if efforts[effort] {
				return fmt.Errorf("agentSettingsPolicy: duplicate effort")
			}
			efforts[effort] = true
		}
	}
	return nil
}

// Allows tests the entire requested selection against one harness's policy.
func (p AgentSettingsPolicy) Allows(harness AgentHarness, target AgentSettings) bool {
	if !p.Enabled || target.Validate() != nil {
		return false
	}
	a, ok := p.Harnesses[harness]
	return ok && slices.Contains(a.Models, target.Model) && slices.Contains(a.Efforts, target.Effort) && (target.FastMode != AgentFastOn || a.AllowFast)
}
