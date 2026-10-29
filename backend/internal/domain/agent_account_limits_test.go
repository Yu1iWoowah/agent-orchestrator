package domain

import (
	"encoding/json"
	"testing"
)

func TestAgentAccountLimitsJSONPreservesWindowLabel(t *testing.T) {
	limits := AgentAccountLimits{Windows: []AgentAccountLimitWindow{{
		ID: "session", Label: "5h", UsedPercent: 25,
	}}}
	encoded, err := json.Marshal(limits)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Windows) != 1 {
		t.Fatalf("expected one window, got %s", encoded)
	}
	window := response.Windows[0]
	if window["label"] != "5h" {
		t.Fatalf("expected label 5h, got %s", encoded)
	}
	if len(window) != 3 || window["id"] != "session" || window["usedPercent"] != float64(25) {
		t.Fatalf("unexpected window fields: %s", encoded)
	}
}
