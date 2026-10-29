package transcriptexport

import (
	"context"
	"encoding/hex"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// CaptureBoundaries derives all owned native cutoffs through verified epoch
// ancestry. The coordinator must already fence intake and quiesce the source.
// Build then rereads these fixed boundaries; capture is not a completeness seal.
func CaptureBoundaries(ctx context.Context, snapshot domain.TranscriptSourceSnapshot, reader ports.NativeTranscriptReader) ([]EpochCutoff, error) {
	if snapshot.Session.Harness != domain.HarnessClaudeCode {
		return nil, incomplete("ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED", "only Claude Code source is supported")
	}
	if snapshot.Session.Kind != domain.KindOrchestrator || snapshot.Session.IsTerminated || snapshot.Fingerprint == "" || reader == nil {
		return nil, incomplete("TRANSCRIPT_SOURCE_INVALID", "a live owned source snapshot is required")
	}
	epochs, err := inventory(snapshot)
	if err != nil {
		return nil, err
	}
	if len(epochs) == 0 || len(epochs) > 64 {
		return nil, incomplete("TRANSCRIPT_EPOCHS_INCOMPLETE", "required native inventory is empty or exceeds capacity")
	}
	segments, _, err := readEpochsMode(ctx, epochs, nil, reader, true)
	if err != nil {
		return nil, err
	}
	cuts := make([]EpochCutoff, 0, len(segments))
	for _, segment := range segments {
		if decoded, err := hex.DecodeString(segment.SHA256); err != nil || len(decoded) != 32 {
			return nil, incomplete("TRANSCRIPT_CUTOFF_INVALID", "capture returned no verified native content hash")
		}

		cuts = append(cuts, EpochCutoff{SHA256: segment.SHA256, NativeID: segment.NativeID, Schema: segment.Schema, Bytes: segment.ByteCutoff, LeafID: segment.LeafID})
	}
	return cuts, nil
}
