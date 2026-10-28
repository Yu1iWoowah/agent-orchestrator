package claudecode

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func verifyTranscriptTail(segment *ports.NativeTranscriptSegment, nodes map[string]ports.NativeTranscriptRecord, lineage []string) error {
	state := "pending"
	prompt := ""
	calls := map[string]bool{}
	results := map[string]bool{}
	for _, id := range lineage {
		r := nodes[id]
		switch r.Type {
		case "user":
			state = "pending"
			if r.State == "interrupted" && r.PromptID == prompt {
				state = "interrupted"
			} else if !r.Meta && !r.Companion {
				prompt = r.PromptID
			}
		case "assistant":
			if state != "interrupted" || r.State != "synthetic" {
				state = r.State
			}
		case "attachment":
			if r.State == "queued" {
				state = "pending"
			}
		}
		for _, b := range r.Blocks {
			switch b.Kind {
			case ports.TranscriptToolCall:
				if calls[b.CallID] {
					return transcriptFailure("TRANSCRIPT_IDENTITY_CONFLICT", "tool call identity repeats")
				}
				calls[b.CallID] = true
			case ports.TranscriptToolResult:
				if !calls[b.CallID] || results[b.CallID] {
					return transcriptFailure("TRANSCRIPT_ANCESTRY_INCOMPLETE", "tool result has no unique preceding call")
				}
				results[b.CallID] = true
			}
		}
	}
	if state != "complete" && state != "interrupted" {
		return transcriptFailure("TRANSCRIPT_TAIL_UNSETTLED", "native main-line tail has not settled")
	}
	for call := range calls {
		if !results[call] {
			segment.Counts.UnresolvedToolCalls++
		}
	}
	if state == "complete" && segment.Counts.UnresolvedToolCalls > 0 {
		return transcriptFailure("TRANSCRIPT_ANCESTRY_INCOMPLETE", "completed native tail has missing tool results")
	}
	segment.TailState = state
	return nil
}
