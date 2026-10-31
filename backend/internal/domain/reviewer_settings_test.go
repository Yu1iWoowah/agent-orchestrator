package domain

import "testing"

func TestReviewerPolicyRejectsUnsupportedOrMalformedAllowance(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness ReviewerHarness
		allow   AgentSettingsAllowance
	}{
		{"unknown reviewer", "not-a-reviewer", AgentSettingsAllowance{}},
		{"Amp mode is not paid fast", ReviewerAmp, AgentSettingsAllowance{AllowFast: true}},
		{"invalid model", ReviewerCodex, AgentSettingsAllowance{Models: []AgentModelSelection{"model\nprivate"}}},
		{"duplicate effort", ReviewerCodex, AgentSettingsAllowance{Efforts: []AgentEffortSelection{"high", "high"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := AgentSettingsPolicy{Enabled: true, Reviewers: map[ReviewerHarness]AgentSettingsAllowance{tc.harness: tc.allow}}
			if err := policy.Validate(); err == nil {
				t.Fatal("invalid reviewer policy was accepted")
			}
		})
	}
}
