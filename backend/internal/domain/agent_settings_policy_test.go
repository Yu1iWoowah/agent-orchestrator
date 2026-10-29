package domain

import (
	"encoding/json"
	"testing"
)

func TestAgentSettingsPolicyRequiresExplicitChoices(t *testing.T) {
	p := AgentSettingsPolicy{Enabled: true, Harnesses: map[AgentHarness]AgentSettingsAllowance{HarnessCodex: {Models: []AgentModelSelection{"allowed"}, Efforts: []AgentEffortSelection{"high"}}}}
	choice := AgentSettings{Model: "allowed", Effort: "high", FastMode: AgentFastOff}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if !p.Allows(HarnessCodex, choice) {
		t.Fatal("explicit standard choice denied")
	}
	for _, bad := range []AgentSettings{
		{Model: "other", Effort: "high", FastMode: AgentFastOff},
		{Model: "allowed", Effort: "low", FastMode: AgentFastOff},
		{Model: "allowed", Effort: "high", FastMode: AgentFastOn},
		{FastMode: AgentFastDefault},
	} {
		if p.Allows(HarnessCodex, bad) {
			t.Fatalf("out-of-policy choice accepted: %+v", bad)
		}
	}
	if p.Allows(HarnessClaudeCode, choice) {
		t.Fatal("borrowed another harness policy")
	}
	p.Enabled = false
	if p.Allows(HarnessCodex, choice) {
		t.Fatal("disabled policy granted permission")
	}
	if (AgentSettingsPolicy{Enabled: true}).Allows(HarnessCodex, choice) {
		t.Fatal("missing allowlist allowed everything")
	}
}

func TestAgentSettingsPolicyPreservesExplicitDefaultAndFastPermission(t *testing.T) {
	p := AgentSettingsPolicy{Enabled: true, Harnesses: map[AgentHarness]AgentSettingsAllowance{HarnessCodex: {Models: []AgentModelSelection{"", "allowed"}, Efforts: []AgentEffortSelection{"", "high"}, AllowFast: true}}}
	for _, fast := range []AgentFastMode{AgentFastDefault, AgentFastOff, AgentFastOn} {
		if !p.Allows(HarnessCodex, AgentSettings{FastMode: fast}) {
			t.Fatalf("explicitly allowed provider default denied: %s", fast)
		}
	}
	cfg := ProjectConfig{AgentSettingsPolicy: &p}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var restored ProjectConfig
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.AgentSettingsPolicy == nil || !restored.AgentSettingsPolicy.Allows(HarnessCodex, AgentSettings{FastMode: AgentFastOn}) {
		t.Fatal("project config lost the explicit default/fast policy")
	}
}

func TestAgentSettingsPolicyRejectsMalformedAllowlist(t *testing.T) {
	for _, p := range []AgentSettingsPolicy{
		{Enabled: true, Harnesses: map[AgentHarness]AgentSettingsAllowance{"unknown": {}}},
		{Enabled: true, Harnesses: map[AgentHarness]AgentSettingsAllowance{HarnessCodex: {Models: []AgentModelSelection{"bad\nmodel"}}}},
		{Enabled: true, Harnesses: map[AgentHarness]AgentSettingsAllowance{HarnessCodex: {Efforts: []AgentEffortSelection{"bad\neffort"}}}},
		{Enabled: true, Harnesses: map[AgentHarness]AgentSettingsAllowance{HarnessAmp: {AllowFast: true}}},
		{Enabled: true, Harnesses: map[AgentHarness]AgentSettingsAllowance{HarnessCodex: {Models: []AgentModelSelection{"same", "same"}}}},
	} {
		if err := (ProjectConfig{AgentSettingsPolicy: &p}).Validate(); err == nil {
			t.Fatalf("malformed policy accepted: %+v", p)
		}
	}
}
