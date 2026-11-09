package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

// HCGProfileRef is an exact approved revision, never a mutable alias.
type HCGProfileRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

// HCGDelegationToggle permits boolean role policy without profile selection.
type HCGDelegationToggle struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// HCGDelegationConfig holds project-only opt-in and exact profile selection.
type HCGDelegationConfig struct {
	Enabled         *bool           `json:"enabled,omitempty"`
	AllowedProfiles []HCGProfileRef `json:"allowedProfiles,omitempty"`
	SelectedProfile *HCGProfileRef  `json:"selectedProfile,omitempty"`
}

// HCGDelegationPolicySnapshot freezes only the inputs of fixed delegation.
type HCGDelegationPolicySnapshot struct {
	Project              HCGDelegationConfig
	Worker, Orchestrator *bool
}

var hcgProfileToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Validate rejects missing, ambiguous or unapproved exact profile selections.
func (c HCGDelegationConfig) Validate() error {
	seen := map[string]bool{}
	for _, ref := range c.AllowedProfiles {
		if !hcgProfileToken.MatchString(ref.ID) || !hcgProfileToken.MatchString(ref.Revision) || seen[ref.ID] {
			return fmt.Errorf("allowedProfiles requires unique IDs with exact revisions")
		}
		seen[ref.ID] = true
	}
	if len(c.AllowedProfiles) > 2 {
		return fmt.Errorf("only the two fixed delegation profiles are supported")
	}
	if c.SelectedProfile != nil {
		found := false
		for _, ref := range c.AllowedProfiles {
			if ref == *c.SelectedProfile {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("selectedProfile must be an allowed exact revision")
		}
	}
	if c.Enabled != nil && *c.Enabled && (len(c.AllowedProfiles) == 0 || c.SelectedProfile == nil) {
		return fmt.Errorf("enabled delegation requires allowedProfiles and selectedProfile")
	}
	return nil
}

// DelegationPolicySnapshot copies policy values without sharing mutable input.
func (c ProjectConfig) DelegationPolicySnapshot() HCGDelegationPolicySnapshot {
	clone := func(v *bool) *bool {
		if v == nil {
			return nil
		}
		snapshotValue := *v
		return &snapshotValue
	}
	project := c.HCGDelegation
	project.Enabled = clone(project.Enabled)
	project.AllowedProfiles = append([]HCGProfileRef(nil), project.AllowedProfiles...)
	if project.SelectedProfile != nil {
		snapshotValue := *project.SelectedProfile
		project.SelectedProfile = &snapshotValue
	}
	return HCGDelegationPolicySnapshot{Project: project, Worker: clone(c.Worker.HCGDelegation.Enabled), Orchestrator: clone(c.Orchestrator.HCGDelegation.Enabled)}
}

// New fixed keys do not inherit encoding/json's case aliases or duplicate-key
// merging. This leaves existing config decoding unchanged.
func decodeDelegationConfig(data []byte, out any, allowed map[string]bool) error {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("delegation policy must be an object")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || seen[key] {
			return fmt.Errorf("unknown or duplicate delegation policy field")
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return fmt.Errorf("invalid delegation policy")
		}
	}
	if _, err := d.Token(); err != nil {
		return fmt.Errorf("invalid delegation policy")
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid delegation policy")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil {
		return fmt.Errorf("invalid delegation policy")
	}
	return nil
}

// UnmarshalJSON rejects unknown, duplicated and case-aliased project fields.
func (c *HCGDelegationConfig) UnmarshalJSON(data []byte) error {
	type wire HCGDelegationConfig
	return decodeDelegationConfig(data, (*wire)(c), map[string]bool{"enabled": true, "allowedProfiles": true, "selectedProfile": true})
}

// UnmarshalJSON accepts only the explicit boolean policy field.
func (c *HCGDelegationToggle) UnmarshalJSON(data []byte) error {
	type wire HCGDelegationToggle
	return decodeDelegationConfig(data, (*wire)(c), map[string]bool{"enabled": true})
}

// UnmarshalJSON accepts only the exact id and revision fields.
func (c *HCGProfileRef) UnmarshalJSON(data []byte) error {
	type wire HCGProfileRef
	return decodeDelegationConfig(data, (*wire)(c), map[string]bool{"id": true, "revision": true})
}
