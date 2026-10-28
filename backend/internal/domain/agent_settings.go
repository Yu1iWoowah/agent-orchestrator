package domain

import (
	"encoding/json"
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
// JSON null resets model/effort; fastMode always names one of the three modes.
type AgentSettingsPatch struct {
	Model    *AgentModelSelection  `json:"model,omitempty"`
	Effort   *AgentEffortSelection `json:"effort,omitempty"`
	FastMode *AgentFastMode        `json:"fastMode,omitempty" enum:"default,on,off" nullable:"false"`
}

// AgentSettingsSources identifies which preference won for each spawn field.
type AgentSettingsSources struct {
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	FastMode string `json:"fastMode"`
}

// SpawnAgentSettingsResolution is the immutable spawn decision. Validation is
// evidence about selection, never a provider-reported effective value.
type SpawnAgentSettingsResolution struct {
	Settings      AgentSettings        `json:"settings"`
	Sources       AgentSettingsSources `json:"sources"`
	Validation    string               `json:"validation"`
	Reason        string               `json:"reason,omitempty"`
	AccountID     string               `json:"accountId,omitempty"`
	EffortCleared bool                 `json:"effortCleared"`
}

// UnmarshalJSON retains field presence, including a provider-default reset.
func (p *AgentSettingsPatch) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return fmt.Errorf("agent settings must be an object")
	}
	var patch AgentSettingsPatch
	for name, raw := range fields {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("invalid %s selection: %w", name, err)
		}
		switch name {
		case "model":
			patch.Model = new(AgentModelSelection(value))
		case "effort":
			patch.Effort = new(AgentEffortSelection(value))
		case "fastMode":
			patch.FastMode = new(AgentFastMode(value))
		default:
			return fmt.Errorf("unknown agent setting %q", name)
		}
	}
	if _, err := (AgentSettings{FastMode: AgentFastDefault}).Apply(patch); err != nil {
		return err
	}
	*p = patch
	return nil
}

// MarshalJSON writes resets as null, so fingerprints preserve omitted vs reset.
func (p AgentSettingsPatch) MarshalJSON() ([]byte, error) {
	if _, err := (AgentSettings{FastMode: AgentFastDefault}).Apply(p); err != nil {
		return nil, err
	}
	fields := make(map[string]any)
	if p.Model != nil {
		fields["model"] = nil
		if *p.Model != "" {
			fields["model"] = *p.Model
		}
	}
	if p.Effort != nil {
		fields["effort"] = nil
		if *p.Effort != "" {
			fields["effort"] = *p.Effort
		}
	}
	if p.FastMode != nil {
		fields["fastMode"] = *p.FastMode
	}
	return json.Marshal(fields)
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
