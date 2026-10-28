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
		if r.State == "continuation-pending" {
			state = "pending"
			continue
		}
		if r.State == "compaction-summary" || r.Excluded != "" && r.Excluded != "ancestor" {
			continue
		}
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
	// Hooks may be appended after the caller's selected reply without entering
	// its parent chain. A pending continuation descending from that leaf is newer
	// evidence; older hooks before a later settled reply do not reopen it.
	metadataChildren := map[string][]ports.NativeTranscriptRecord{}
	for _, record := range nodes {
		if record.Excluded == "metadata" {
			metadataChildren[record.ParentID] = append(metadataChildren[record.ParentID], record)
		}
	}
	anchored := metadataDescendants(metadataChildren, lineage)
	for _, record := range nodes {
		if record.State == "continuation-pending" && record.NativeID == segment.NativeID && (record.ParentID == "" || !anchored[record.ParentID]) {
			return transcriptFailure("TRANSCRIPT_TAIL_UNSETTLED", "pending hook has no verified lineage parent")
		}
	}
	if len(lineage) > 0 {
		afterLeaf := metadataDescendants(metadataChildren, lineage[len(lineage)-1:])
		for _, record := range nodes {
			if record.State == "continuation-pending" && afterLeaf[record.ID] {
				state = "pending"
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

func metadataDescendants(children map[string][]ports.NativeTranscriptRecord, seeds []string) map[string]bool {
	seen := make(map[string]bool, len(seeds))
	pending := append([]string(nil), seeds...)
	for _, id := range seeds {
		seen[id] = true
	}
	for i := 0; i < len(pending); i++ {
		for _, record := range children[pending[i]] {
			if !seen[record.ID] {
				seen[record.ID] = true
				pending = append(pending, record.ID)
			}
		}
	}
	return seen
}
