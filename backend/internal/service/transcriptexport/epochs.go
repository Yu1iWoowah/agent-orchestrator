package transcriptexport

import (
	"context"
	"errors"
	"slices"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// readEpochs resolves only verified owned ancestry, never an arbitrary file or
// a text match. An unresolved dependency cycle fails instead of being omitted.
func readEpochs(ctx context.Context, epochs map[string]string, cuts map[string]EpochCutoff, reader ports.NativeTranscriptReader) ([]ports.NativeTranscriptSegment, map[string]ports.NativeTranscriptRequest, error) {
	ids := make([]string, 0, len(epochs))
	for id := range epochs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	pending := append([]string(nil), ids...)
	requests := map[string]ports.NativeTranscriptRequest{}
	var segments []ports.NativeTranscriptSegment
	var ancestors []ports.NativeTranscriptRecord
	for len(pending) > 0 {
		var deferred []string
		progress := false
		for _, id := range pending {
			c := cuts[id]
			request := ports.NativeTranscriptRequest{Source: ports.NativeSessionRef{NativeSessionID: id, ConfigDir: epochs[id]}, Schema: c.Schema, ByteCutoff: c.Bytes, LeafID: c.LeafID, KnownNativeIDs: ids, VerifiedAncestors: ancestors}
			segment, e := reader.ReadNativeTranscript(ctx, request)
			if e != nil {
				var nativeError *ports.NativeTranscriptError
				if errors.As(e, &nativeError) && nativeError.Code == "TRANSCRIPT_ANCESTOR_REQUIRED" {
					deferred = append(deferred, id)
					continue
				}
				return nil, nil, e
			}
			// Ancestors is append-only; retaining the immutable prefix avoids a
			// quadratic copy for many epochs. Readers receive values, never mutate it.
			requests[id] = request
			segments = append(segments, segment)
			progress = true
			for _, record := range segment.Records {
				if record.Provenance != domain.NativeThreadMain || record.Excluded != "" && record.Excluded != "metadata" && record.Excluded != "compaction-summary" || record.ID == "" {
					continue
				}
				proof := record
				proof.Blocks = append([]ports.NativeTranscriptBlock(nil), record.Blocks...)
				for i := range proof.Blocks {
					proof.Blocks[i].Text = ""
				}
				ancestors = append(ancestors, proof)
			}
		}
		if !progress {
			return nil, nil, incomplete("TRANSCRIPT_ANCESTRY_INCOMPLETE", "required native epochs have unresolved parent dependencies")
		}
		pending = deferred
	}
	return segments, requests, nil
}
