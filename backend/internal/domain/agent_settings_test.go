package domain

import (
	"encoding/json"
	"testing"
)

func TestAgentSettingsPatchPreservesOmittedAndResetsExplicitDefaults(t *testing.T) {
	base := AgentSettings{Model: "model-a", Effort: "high", FastMode: AgentFastOn}
	resetModel := AgentModelSelection("")
	resetEffort := AgentEffortSelection("")
	off := AgentFastOff
	for _, tc := range []struct {
		name  string
		patch AgentSettingsPatch
		want  AgentSettings
	}{
		{"omitted", AgentSettingsPatch{}, base},
		{"reset model", AgentSettingsPatch{Model: &resetModel}, AgentSettings{Effort: "high", FastMode: AgentFastOn}},
		{"reset effort", AgentSettingsPatch{Effort: &resetEffort}, AgentSettings{Model: "model-a", FastMode: AgentFastOn}},
		{"explicit off", AgentSettingsPatch{FastMode: &off}, AgentSettings{Model: "model-a", Effort: "high", FastMode: AgentFastOff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := base.Apply(tc.patch)
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestAgentSettingsRejectsInvalidSelection(t *testing.T) {
	for _, settings := range []AgentSettings{
		{FastMode: "turbo"}, {Model: "bad\nmodel", FastMode: AgentFastDefault}, {Effort: "high\x00", FastMode: AgentFastDefault},
	} {
		if err := settings.Validate(); err == nil {
			t.Fatalf("accepted %+v", settings)
		}
	}
}

func TestAgentSettingsJSONKeepsExplicitFastOffAndDefault(t *testing.T) {
	for _, mode := range []AgentFastMode{AgentFastDefault, AgentFastOn, AgentFastOff} {
		settings := AgentSettings{FastMode: mode}
		raw, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		var got AgentSettings
		if err = json.Unmarshal(raw, &got); err != nil || got != settings {
			t.Fatalf("roundtrip %s: %+v, %v", raw, got, err)
		}
		var fields map[string]any
		if err = json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["fastMode"] != string(mode) {
			t.Fatalf("fast lost: %s", raw)
		}
	}
}

func TestAgentFastModeFromLegacyLaunch(t *testing.T) {
	for _, tc := range []struct {
		launch  AgentLaunchOptions
		mode    AgentFastMode
		invalid bool
	}{
		{AgentLaunchOptions{}, AgentFastDefault, false},
		{AgentLaunchOptions{FastMode: true}, AgentFastOn, false},
		{AgentLaunchOptions{FastModeOff: true}, AgentFastOff, false},
		{AgentLaunchOptions{FastMode: true, FastModeOff: true}, "", true},
	} {
		got, err := AgentFastModeFromLaunch(tc.launch)
		if (err != nil) != tc.invalid || got != tc.mode {
			t.Fatalf("%+v: %q, %v", tc.launch, got, err)
		}
	}
}
