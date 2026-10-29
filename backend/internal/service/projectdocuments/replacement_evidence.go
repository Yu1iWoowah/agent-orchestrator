package projectdocuments

import (
	"context"
	"maps"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

// PublishReplacementEvidence is a daemon-internal operation-bound publication.
// It authenticates persisted ownership instead of fabricating a raw capability.
func (s *Service) PublishReplacementEvidence(ctx context.Context, operation string, revision int64, out transcriptexport.Output) (domain.OrchestratorReplacement, error) {
	var zero domain.OrchestratorReplacement
	store, ok := s.store.(replacementStore)
	if !ok {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_UNAVAILABLE", "Replacement publication store is unavailable")
	}
	op, found, err := store.GetOrchestratorReplacement(ctx, operation)
	if err != nil {
		return zero, err
	}
	t, p, stop := op.Transcript, op.Preparation, op.SourceStop
	if !found || op.Revision != revision || op.Phase != domain.ReplacementExportingTranscript || op.SourceRetiredAt != nil || op.FrozenAt == nil || op.SourceStopIntentAt == nil || t == nil || p == nil || p.VerifiedAt == nil || stop == nil || stop.ConfirmedAt.IsZero() || stop.Owner != op.SourceOwner || stop.CapabilityVerifier != op.SourceCapabilityVerifier || t.CapturedAt == nil || t.ExportedAt == nil || t.CapturedAt.IsZero() || t.ExportedAt.IsZero() || len(t.NativeCutoffs) == 0 || t.SnapshotFingerprint == "" {
		return zero, conflict("HANDOFF_EVIDENCE_UNVERIFIED", "A frozen, quiesced source and durable native capture are required")
	}
	if !out.Valid() || out.Manifest.SnapshotFingerprint != t.SnapshotFingerprint || !maps.Equal(out.Manifest.ConversationCutoffs, t.AO.ConversationCutoffs) || len(out.Manifest.Segments) != len(t.NativeCutoffs) {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Export does not match the captured operation boundary")
	}
	cuts := map[string]domain.TranscriptEpochCutoff{}
	for _, cut := range t.NativeCutoffs {
		if _, duplicate := cuts[cut.NativeID]; duplicate {
			return zero, conflict("HANDOFF_EVIDENCE_UNVERIFIED", "Captured epoch identity is ambiguous")
		}
		cuts[cut.NativeID] = cut
	}
	for _, segment := range out.Manifest.Segments {
		cut, ok := cuts[segment.NativeID]
		if !ok || cut.SHA256 != segment.SHA256 || cut.Bytes != segment.ByteCutoff || cut.LeafID != segment.LeafID || cut.Schema != segment.Schema {
			return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Native export differs from the captured epoch boundary")
		}
	}
	in := EvidenceInput{Actor: Actor{SessionID: op.SourceSessionID}, Lease: Grant{Epoch: p.LeaseEpoch}, OperationID: op.ID, AttemptID: t.AttemptID, HandoffPath: op.DocumentPath, HandoffHash: p.SubmittedHash, Export: out, CreatedAt: *t.ExportedAt}
	_, err = s.publishEvidence(ctx, op.ProjectID, in, &op)
	if err != nil {
		return zero, err
	}
	op.Revision++
	return op, nil
}

func (s *Service) replacementEvidenceSource(ctx context.Context, op domain.OrchestratorReplacement, state domain.ProjectDocumentState) (domain.SessionRecord, error) {
	var zero domain.SessionRecord
	source, found, err := s.store.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		return zero, err
	}
	lease := state.Lease
	if !found || source.IsTerminated || source.ControllerOwner() != op.SourceOwner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier || op.SourceCapabilityVerifier == "" || lease == nil || !lease.Frozen || lease.ReplacementID != op.ID || lease.Owner != op.SourceOwner || lease.Epoch != op.Preparation.LeaseEpoch || lease.TokenHash == "" {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Frozen source ownership changed")
	}
	if op.SourceStop == nil || !op.SourceStop.MatchesSource(source) {
		return zero, conflict("HANDOFF_EVIDENCE_UNVERIFIED", "Source shutdown is not confirmed for the current controller")
	}
	return source, nil
}
