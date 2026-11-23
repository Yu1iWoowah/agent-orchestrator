package integration

import "testing"

func boolean(b bool) *bool { return &b }

func TestPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, role string
		in         PolicyInputs
		want       Policy
	}{
		{"default", "worker", PolicyInputs{}, Policy{Enabled: false, Source: "default"}},
		{"project", "worker", PolicyInputs{Project: boolean(true)}, Policy{Enabled: true, Source: "project"}},
		{"role off", "worker", PolicyInputs{Project: boolean(true), Role: boolean(false)}, Policy{Enabled: false, Source: "role"}},
		{"session on", "orchestrator", PolicyInputs{Role: boolean(false), Session: boolean(true)}, Policy{Enabled: true, Source: "session"}},
		{"session off", "worker", PolicyInputs{Project: boolean(true), Session: boolean(false)}, Policy{Enabled: false, Source: "session"}},
		{"clear parent on", "worker", PolicyInputs{Project: boolean(true)}, Policy{Enabled: true, Source: "project"}},
		{"clear parent off", "worker", PolicyInputs{Project: boolean(false)}, Policy{Enabled: false, Source: "project"}},
		{"stop", "worker", PolicyInputs{Session: boolean(true), EmergencyStop: true}, Policy{Enabled: false, Source: "emergency_stop"}},
		{"reviewer", "reviewer", PolicyInputs{Project: boolean(true), Session: boolean(true)}, Policy{Enabled: false, Source: "unsupported_role"}},
		{"side", "side", PolicyInputs{Project: boolean(true)}, Policy{Enabled: false, Source: "unsupported_role"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolvePolicy(tc.role, tc.in); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}
