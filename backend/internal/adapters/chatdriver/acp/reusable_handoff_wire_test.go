//go:build linux

package acp

import (
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	rh "github.com/aoagents/agent-orchestrator/backend/internal/test/reusablehandoff"
)

func TestReusableHandoffProducer(t *testing.T) {
	rh.RunSuite(t, func(input rh.Input) ports.ChatEvent {
		status := acpsdk.ToolCallStatusInProgress
		meta := map[string]any{}
		if input.ExitCode != nil {
			status = acpsdk.ToolCallStatusCompleted
			meta["terminal_exit"] = map[string]any{"terminal_id": input.ID, "exit_code": *input.ExitCode}
		}
		tool := &toolState{id: input.ID, kind: acpsdk.ToolKindExecute, status: status, rawInput: map[string]any{"command": input.Command}, terminalOutput: input.Stdout, meta: meta,
			content: []acpsdk.ToolCallContent{{Terminal: &acpsdk.ToolCallContentTerminal{Type: "terminal", TerminalId: input.ID}}}}
		return (&conversation{}).toolEvent(input.Turn, tool, input.ExitCode != nil)
	})
}
