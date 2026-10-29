package transcriptexport

import (
	"context"
	"errors"
	"slices"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// RequiredNativeEpochs shares the exporter inventory with daemon preflight and
// cutoff capture. It never starts a controller or reads native transcript bytes.
func RequiredNativeEpochs(snapshot domain.TranscriptSourceSnapshot) (map[string]string, error) {
	return inventory(snapshot)
}

// readEpochs resolves only verified owned ancestry, never an arbitrary file or
// a text match. An unresolved dependency cycle fails instead of being omitted.
func readEpochs(ctx context.Context, epochs map[string]string, cuts map[string]EpochCutoff, reader ports.NativeTranscriptReader) ([]ports.NativeTranscriptSegment, map[string]ports.NativeTranscriptRequest, error) {
	return readEpochsMode(ctx, epochs, cuts, reader, false)
}

func readEpochsMode(ctx context.Context, epochs map[string]string, cuts map[string]EpochCutoff, reader ports.NativeTranscriptReader, capture bool) ([]ports.NativeTranscriptSegment, map[string]ports.NativeTranscriptRequest, error) {
	capturer, canCapture := reader.(ports.NativeTranscriptCapturer)
	if capture && !canCapture {
		return nil, nil, incomplete("TRANSCRIPT_CAPTURE_UNAVAILABLE", "reader cannot derive native boundaries")
	}
	var capturedBytes int64
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
			if capture {
				c.Schema = ports.ClaudeTranscriptSchemaV1
			}
			request := ports.NativeTranscriptRequest{Source: ports.NativeSessionRef{NativeSessionID: id, ConfigDir: epochs[id]}, Schema: c.Schema, ByteCutoff: c.Bytes, LeafID: c.LeafID, KnownNativeIDs: ids, VerifiedAncestors: ancestors}
			var segment ports.NativeTranscriptSegment
			var e error
			if capture {
				remaining := int64(ports.MaxTranscriptSourceBytes) - capturedBytes
				if remaining <= 0 {
					return nil, nil, incomplete("TRANSCRIPT_LIMIT_EXCEEDED", "combined native capture exceeds capacity")
				}
				request.MaxSourceBytes = remaining
				segment, e = capturer.CaptureNativeTranscript(ctx, request)
			} else {
				segment, e = reader.ReadNativeTranscript(ctx, request)
			}
			if e != nil {
				var nativeError *ports.NativeTranscriptError
				if errors.As(e, &nativeError) && nativeError.Code == "TRANSCRIPT_ANCESTOR_REQUIRED" {
					deferred = append(deferred, id)
					continue
				}
				return nil, nil, e
			}
			if segment.NativeID != id {
				return nil, nil, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "reader returned a different native epoch")
			}
			if capture {
				if segment.Schema != ports.ClaudeTranscriptSchemaV1 || segment.LeafID == "" || segment.ByteCutoff <= 0 || segment.TailState != "complete" && segment.TailState != "interrupted" {
					return nil, nil, incomplete("TRANSCRIPT_CUTOFF_INVALID", "capture returned no verified settled boundary")
				}
				if segment.ByteCutoff > int64(ports.MaxTranscriptSourceBytes)-capturedBytes {
					return nil, nil, incomplete("TRANSCRIPT_LIMIT_EXCEEDED", "combined native capture exceeds capacity")
				}
				capturedBytes += segment.ByteCutoff
				request.ByteCutoff, request.LeafID = segment.ByteCutoff, segment.LeafID
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
