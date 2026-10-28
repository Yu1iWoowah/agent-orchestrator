package domain

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// AgentModelSelection and AgentEffortSelection are session preferences. Empty
// explicitly selects the provider default, not today's project/role default.
type AgentModelSelection string

// AgentEffortSelection selects a provider reasoning level; empty resets it.
type AgentEffortSelection string

// AgentFastMode distinguishes an explicit standard tier from provider defaults.
type AgentFastMode string

// Agent fast preferences, including an explicit provider default.
const (
	AgentFastDefault AgentFastMode = "default"
	AgentFastOn      AgentFastMode = "on"
	AgentFastOff     AgentFastMode = "off"
)

// AgentSettings describes a committed preference, never a claim about the
// provider's effective model or the settings of an already dispatched turn.
type AgentSettings struct {
	Model    AgentModelSelection  `json:"model"`
	Effort   AgentEffortSelection `json:"effort"`
	FastMode AgentFastMode        `json:"fastMode" enum:"default,on,off"`
}

// AgentSettingsPatch uses nil for omitted and a pointer to empty for reset.
// HTTP decoders must preserve that distinction rather than decoding JSON null
// directly into these pointers.
type AgentSettingsPatch struct {
	Model    *AgentModelSelection
	Effort   *AgentEffortSelection
	FastMode *AgentFastMode
}

// Apply changes only supplied fields. Provider capability validation is separate.
func (s AgentSettings) Apply(p AgentSettingsPatch) (AgentSettings, error) {
	if p.Model != nil {
		s.Model = *p.Model
	}
	if p.Effort != nil {
		s.Effort = *p.Effort
	}
	if p.FastMode != nil {
		s.FastMode = *p.FastMode
	}
	return s, s.Validate()
}

// Validate checks preference syntax without claiming provider support.
func (s AgentSettings) Validate() error {
	if s.FastMode != AgentFastDefault && s.FastMode != AgentFastOn && s.FastMode != AgentFastOff {
		return fmt.Errorf("invalid fast mode %q", s.FastMode)
	}
	for _, v := range []struct {
		name, value string
		limit       int
	}{{"model", string(s.Model), 256}, {"effort", string(s.Effort), 64}} {
		if !utf8.ValidString(v.value) || len(v.value) > v.limit || strings.TrimSpace(v.value) != v.value || strings.ContainsFunc(v.value, unicode.IsControl) {
			return fmt.Errorf("invalid %s selection", v.name)
		}
	}
	return nil
}

// AgentFastModeFromLaunch accepts the legacy launch JSON only at the boundary.
// Contradictory historical values are refused instead of choosing a paid tier.
func AgentFastModeFromLaunch(options AgentLaunchOptions) (AgentFastMode, error) {
	if options.FastMode && options.FastModeOff {
		return "", fmt.Errorf("conflicting legacy fast mode choices")
	}
	if options.FastMode {
		return AgentFastOn, nil
	}
	if options.FastModeOff {
		return AgentFastOff, nil
	}
	return AgentFastDefault, nil
}
