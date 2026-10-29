package claudecode

import (
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestClaudeSpawnFastIsSessionLocalSettings(t *testing.T) {
	for _, on := range []bool{false, true} {
		args := claudeLaunchSettingsArgs(domain.AgentLaunchOptions{FastMode: on, FastModeOff: !on, Ultracode: true}, "")
		if len(args) != 2 || args[0] != "--settings" {
			t.Fatalf("args=%q", args)
		}
		var settings map[string]any
		if err := json.Unmarshal([]byte(args[1]), &settings); err != nil {
			t.Fatal(err)
		}
		if settings["fastMode"] != on || settings["ultracode"] != true {
			t.Fatalf("settings=%s want fast=%v", args[1], on)
		}
	}
}
