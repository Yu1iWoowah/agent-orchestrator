package claudeacp

import (
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSpawnFastMapsToClaudeSessionOption(t *testing.T) {
	for _, mode := range []string{"on", "off", "default"} {
		var settings ports.ChatTurnSettings
		if err := json.Unmarshal([]byte(`{"FastMode":"`+mode+`"}`), &settings); err != nil {
			t.Fatal(err)
		}
		options := claudeSessionOptions(settings)
		if mode == "default" {
			if len(options) != 0 {
				t.Fatalf("default overrode options=%v", options)
			}
			continue
		}
		if len(options) != 1 || options[0].ID != "fast" || options[0].Value != mode {
			t.Fatalf("mode=%s options=%v", mode, options)
		}
	}
}
