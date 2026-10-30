package projectdocuments

import (
	"context"
	"io"
	"path/filepath"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ReadReplacementHandoff reads the frozen snapshot for the bootstrap control
// turn. It verifies all published evidence without acquiring a writer lease or
// reading the now-retired source's live native conversation.
func (s *Service) ReadReplacementHandoff(ctx context.Context, operation string, revision int64) (File, error) {
	var zero File
	store, ok := s.store.(replacementStore)
	if !ok {
		return zero, domain.ErrReplacementStateInvalid
	}
	op, found, err := store.GetOrchestratorReplacement(ctx, operation)
	if err != nil {
		return zero, err
	}
	t, p := op.Transcript, op.Preparation
	if !found || op.Revision != revision || op.Phase != domain.ReplacementControllerReady || op.SourceRetiredAt == nil || op.FrozenAt == nil || t == nil || t.ExportedAt == nil || t.Receipt == nil || t.TranscriptHash == "" || p == nil || p.VerifiedAt == nil || p.SubmittedHash == "" {
		return zero, conflict("HANDOFF_EVIDENCE_UNVERIFIED", "Bootstrap requires retired source and verified immutable evidence")
	}
	if err := evidenceIDs(op.ID, t.AttemptID); err != nil {
		return zero, err
	}
	dir := s.evidenceDirectory(op.ProjectID, op.ID, t.AttemptID)
	if dir != t.Receipt.Directory {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Evidence directory differs from the operation")
	}
	unlock := s.lock(op.ProjectID)
	defer unlock()
	state, shared, doc, err := s.state(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	lease := state.Lease
	if shared != op.SharedDir || doc != op.DocumentPath || lease == nil || !lease.Frozen || lease.ReplacementID != op.ID || lease.Owner != op.SourceOwner || lease.Epoch != p.LeaseEpoch || state.Evidence[op.ID+"/"+t.AttemptID] != *t.Receipt {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Frozen evidence ownership changed")
	}
	if hash([]byte(p.SubmittedHash+"\x00"+t.Receipt.ManifestHash+"\x00"+t.TranscriptHash)) != t.Receipt.InputHash {
		return zero, conflict("HANDOFF_EVIDENCE_UNVERIFIED", "Evidence receipt does not bind the bootstrap inputs")
	}
	witnesses := make([]retirementFileWitness, 0, 3)
	for _, entry := range []struct {
		name, digest string
		limit        int64
	}{
		{"handoff.md", p.SubmittedHash, domain.MaxReplacementHandoffBytes},
		{"transcript.md", t.TranscriptHash, 512 << 20},
		{"manifest.json", t.Receipt.ManifestHash, 256 << 20},
	} {
		witness, err := s.captureRetirementFile(ctx, dir, entry.name, entry.digest, -1, entry.limit)
		if err != nil {
			return zero, err
		}
		witnesses = append(witnesses, witness)
	}
	r, err := s.openRoot(dir)
	if err != nil {
		return zero, err
	}
	defer func() { _ = r.Close() }()
	f, err := openFile(r, "handoff.md")
	if err != nil {
		return zero, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, domain.MaxReplacementHandoffBytes+1))
	if err != nil {
		return zero, err
	}
	if len(b) > domain.MaxReplacementHandoffBytes || !utf8.Valid(b) || hash(b) != p.SubmittedHash {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Immutable handoff bytes changed")
	}
	for _, witness := range witnesses {
		if err := witness.check(s); err != nil {
			return zero, err
		}
	}
	current, found, err := store.GetOrchestratorReplacement(ctx, operation)
	if err != nil {
		return zero, err
	}
	if !found || current.Revision != revision || current.Phase != op.Phase {
		return zero, domain.ErrReplacementStateInvalid
	}
	return File{Path: filepath.Join(dir, "handoff.md"), Content: string(b), Hash: p.SubmittedHash, Size: int64(len(b))}, nil
}
