package domain

import (
	"encoding/json"
	"testing"
)

func TestDelegationPolicyExactReferencesAndRoleBoundary(t *testing.T) {
	for _, raw := range []string{
		`{"enabled":true}`, `{"enabled":true,"allowedProfiles":[{"id":"p","revision":"v1"},{"id":"p","revision":"v2"}],"selectedProfile":{"id":"p","revision":"v1"}}`,
		`{"allowedProfiles":[{"id":"p","revision":"v1"}],"selectedProfile":{"id":"p","revision":"v2"}}`,
		`{"enabled":true,"provider":"openai"}`, `{"Enabled":true}`, `{"enabled":false,"enabled":true}`,
	} {
		var cfg HCGDelegationConfig
		err := json.Unmarshal([]byte(raw), &cfg)
		if err == nil {
			err = cfg.Validate()
		}
		if err == nil {
			t.Fatalf("invalid delegation config accepted: %s", raw)
		}
	}
	var cfg ProjectConfig
	if err := json.Unmarshal([]byte(`{"hcgDelegation":{"enabled":true,"allowedProfiles":[{"id":"p","revision":"v1"}],"selectedProfile":{"id":"p","revision":"v1"}},"worker":{"hcgDelegation":{"enabled":false}}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	snapshot := cfg.DelegationPolicySnapshot()
	*cfg.HCGDelegation.Enabled = false
	cfg.HCGDelegation.AllowedProfiles[0].Revision = "changed"
	if !*snapshot.Project.Enabled || snapshot.Project.AllowedProfiles[0].Revision != "v1" {
		t.Fatal("snapshot aliases mutable config")
	}
	if err := json.Unmarshal([]byte(`{"worker":{"hcgDelegation":{"enabled":true,"selectedProfile":{"id":"p","revision":"v1"}}}}`), &cfg); err == nil {
		t.Fatal("role selected a provider")
	}
}
