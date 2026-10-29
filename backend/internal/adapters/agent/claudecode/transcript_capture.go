package claudecode

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.NativeTranscriptCapturer = (*Plugin)(nil)

// CaptureNativeTranscript derives the settled main-line EOF boundary without
// mutating the provider or treating child/metadata records as main content.
func (p *Plugin) CaptureNativeTranscript(ctx context.Context, request ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	if request.ByteCutoff != 0 || request.LeafID != "" {
		return ports.NativeTranscriptSegment{}, transcriptFailure("TRANSCRIPT_CUTOFF_INVALID", "capture derives its boundary; caller must not select a leaf")
	}
	return p.readNativeTranscript(ctx, request, true)
}

// captureMainLeaf visits each ancestor at most once. Metadata descendants are
// checked by verifyTranscriptTail but cannot choose the readable cutoff leaf.
func captureMainLeaf(nodes map[string]ports.NativeTranscriptRecord, native string) (string, error) {
	queue := make([]string, 0, len(nodes))
	enqueued := make(map[string]bool, len(nodes))
	for id, r := range nodes {
		if r.NativeID == native && r.Provenance == domain.NativeThreadMain && r.Excluded == "" {
			queue = append(queue, id)
			enqueued[id] = true
		}
	}
	hasDescendant := make(map[string]bool, len(nodes))
	for i := 0; i < len(queue); i++ {
		parent := nodes[queue[i]].ParentID
		if parent == "" {
			continue
		}
		hasDescendant[parent] = true
		if _, found := nodes[parent]; found && !enqueued[parent] {
			queue = append(queue, parent)
			enqueued[parent] = true
		}
	}
	leaf := ""
	for id, r := range nodes {
		if r.NativeID != native || r.Provenance != domain.NativeThreadMain || r.Excluded != "" || hasDescendant[id] {
			continue
		}
		if leaf != "" {
			return "", transcriptFailure("TRANSCRIPT_CUTOFF_AMBIGUOUS", "native main line has multiple readable leaves")
		}
		leaf = id
	}
	if leaf == "" {
		return "", transcriptFailure("TRANSCRIPT_ANCESTRY_INCOMPLETE", "native main line has no readable leaf")
	}
	return leaf, nil
}
