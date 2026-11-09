//go:build linux

package codexappserver

import (
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	rh "github.com/aoagents/agent-orchestrator/backend/internal/test/reusablehandoff"
)

func TestReusableHandoffProducer(t *testing.T) {
	rh.RunSuite(t, func(input rh.Input) ports.ChatEvent {
		item := map[string]any{"id": input.ID, "type": "commandExecution", "command": input.Command, "aggregatedOutput": input.Stdout}
		completed := input.ExitCode != nil
		if completed {
			item["exitCode"] = *input.ExitCode
		}
		raw, err := json.Marshal(map[string]any{"threadId": "synthetic", "turnId": input.Turn, "item": item})
		if err != nil {
			t.Fatal(err)
		}
		events := normalizeItem(raw, completed)
		if len(events) != 1 {
			t.Fatalf("normalizer returned %d events", len(events))
		}
		return events[0]
	})
}
