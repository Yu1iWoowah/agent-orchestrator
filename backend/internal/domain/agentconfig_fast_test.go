package domain

import (
	"encoding/json"
	"testing"
)

func TestAgentConfigFastModeValidation(t *testing.T) {
	for _, mode := range []string{"", "default", "on", "off", "turbo"} {
		var cfg AgentConfig
		if err := json.Unmarshal([]byte(`{"fastMode":"`+mode+`"}`), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); (err != nil) != (mode == "turbo") {
			t.Fatalf("mode=%q validation=%v", mode, err)
		}
		if cfg.IsZero() != (mode == "") {
			t.Fatalf("mode=%q zero=%v", mode, cfg.IsZero())
		}
	}
}
