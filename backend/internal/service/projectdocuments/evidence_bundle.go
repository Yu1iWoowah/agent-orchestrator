package projectdocuments

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type evidenceBundle struct {
	transcript []byte
	manifest   []byte
	receipt    domain.HandoffEvidenceReceipt
}

// Use identical bytes for create-only publication and later retirement checks.
func (s *Service) buildEvidenceBundle(id domain.ProjectID, in EvidenceInput, state domain.ProjectDocumentState, shared string, handoff []byte) (evidenceBundle, error) {
	transcript := []byte(fmt.Sprintf("Operation: %s\nAttempt: %s\nExport time: %s\nKnown message time range: %s — %s\n\n", in.OperationID, in.AttemptID, in.CreatedAt.UTC().Format(time.RFC3339Nano), in.Export.Manifest.TimeStart, in.Export.Manifest.TimeEnd))
	transcript = append(transcript, in.Export.Transcript...)
	manifest := EvidenceManifest{Manifest: in.Export.Manifest, OperationID: in.OperationID, AttemptID: in.AttemptID, ExportedAt: in.CreatedAt.UTC(), SharedDirectory: shared, HandoffPath: in.HandoffPath, LeaseEpoch: in.Lease.Epoch, Files: []domain.ProjectDocumentEntry{{Path: "handoff.md", Hash: hash(handoff), Size: int64(len(handoff))}, {Path: "transcript.md", Hash: hash(transcript), Size: int64(len(transcript))}}}
	if state.Adoption != nil {
		manifest.AdoptionManifestHash = state.Adoption.ManifestHash
	}
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return evidenceBundle{}, err
	}
	mb = append(mb, '\n')
	if len(transcript) > 512<<20 || len(mb) > 256<<20 {
		return evidenceBundle{}, conflict("HANDOFF_EVIDENCE_TOO_LARGE", "Evidence exceeds publication capacity")
	}
	receipt := domain.HandoffEvidenceReceipt{Directory: s.evidenceDirectory(id, in.OperationID, in.AttemptID), ManifestHash: hash(mb), InputHash: hash([]byte(in.HandoffHash + "\x00" + hash(mb) + "\x00" + hash(transcript)))}
	return evidenceBundle{transcript: transcript, manifest: mb, receipt: receipt}, nil
}
