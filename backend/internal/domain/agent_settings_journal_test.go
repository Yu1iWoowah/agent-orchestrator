package domain

import "testing"

func TestAgentSettingsCheckpointOrder(t *testing.T) {
	for _, tc := range []struct {
		from, to AgentSettingsCheckpoint
		want     bool
	}{
		{AgentSettingsQueued, AgentSettingsApplyingFast, true},
		{AgentSettingsApplyingModel, AgentSettingsApplyingEffort, true},
		{AgentSettingsApplyingFast, AgentSettingsApplyingModel, false},
		{AgentSettingsVerifying, AgentSettingsVerified, true},
		{AgentSettingsApplyingModel, AgentSettingsVerified, false},
		{AgentSettingsVerified, AgentSettingsRollingBack, true},
		{AgentSettingsRollingBack, AgentSettingsRestored, true},
		{AgentSettingsQueued, AgentSettingsRollingBack, false},
		{AgentSettingsRestored, AgentSettingsApplyingFast, false},
		{"unknown", AgentSettingsRollingBack, false},
	} {
		if got := tc.from.CanAdvanceTo(tc.to); got != tc.want {
			t.Errorf("%s -> %s = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}
