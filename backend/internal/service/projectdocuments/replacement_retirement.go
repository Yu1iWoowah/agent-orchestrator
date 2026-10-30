package projectdocuments

import (
	"context"
	"io"
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/filefingerprint"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

// ReplacementRetirementProof binds verified bytes to a final, bounded check.
// It cannot be constructed by callers or serialized into a durable authority.
type ReplacementRetirementProof struct {
	operation        string
	revision         int64
	source           domain.SessionRecord
	documentRevision int64
	check            func(context.Context) error
}

// OperationID identifies the verified replacement intent.
func (p ReplacementRetirementProof) OperationID() string { return p.operation }

// OperationRevision is the durable revision verified outside the reservation.
func (p ReplacementRetirementProof) OperationRevision() int64 { return p.revision }

// Source returns the controller snapshot bound to the proof.
func (p ReplacementRetirementProof) Source() domain.SessionRecord { return p.source }

// DocumentRevision binds the frozen lease and published evidence receipt.
func (p ReplacementRetirementProof) DocumentRevision() int64 { return p.documentRevision }

// Check refuses changed native files, evidence files or AO source facts.
func (p ReplacementRetirementProof) Check(ctx context.Context) error {
	if p.check == nil {
		return conflict("HANDOFF_EVIDENCE_UNVERIFIED", "Retirement evidence has not been verified")
	}
	return p.check(ctx)
}

// PrepareReplacementRetirement performs full native and evidence verification
// outside the store writer reservation. Check performs only bounded file
// observations and the durable AO snapshot comparison inside that reservation.
func (s *Service) PrepareReplacementRetirement(ctx context.Context, operation string, revision int64, out transcriptexport.Output) (ReplacementRetirementProof, error) {
	var zero ReplacementRetirementProof
	store, ok := s.store.(replacementStore)
	if !ok {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_UNAVAILABLE", "Replacement document store is unavailable")
	}
	op, found, err := store.GetOrchestratorReplacement(ctx, operation)
	if err != nil {
		return zero, err
	}
	t, p := op.Transcript, op.Preparation
	if !found || op.Revision != revision || op.Phase != domain.ReplacementRetiring || op.SourceRetiredAt != nil || op.FrozenAt == nil || op.SourceStopIntentAt == nil || t == nil || p == nil || p.VerifiedAt == nil || t.CapturedAt == nil || t.ExportedAt == nil || t.Receipt == nil || t.TranscriptHash == "" {
		return zero, conflict("HANDOFF_EVIDENCE_UNVERIFIED", "Retirement requires frozen, published evidence and a stopped source")
	}
	if err := evidenceIDs(op.ID, t.AttemptID); err != nil {
		return zero, err
	}
	if !out.Valid() || out.Manifest.ProjectID != op.ProjectID || out.Manifest.SourceID != op.SourceSessionID || out.Manifest.Owner != op.SourceOwner || out.Manifest.SnapshotFingerprint != t.SnapshotFingerprint {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Retirement export differs from the published source")
	}
	unlock := s.lock(op.ProjectID)
	defer unlock()
	state, shared, doc, err := s.state(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	if shared != op.SharedDir || doc != op.DocumentPath {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Configured handoff location changed")
	}
	if state.Adoption != nil && !state.Adoption.Complete {
		return zero, conflict("PROJECT_DOCUMENT_ADOPTION_PENDING", "Living file adoption is incomplete")
	}
	source, err := s.replacementEvidenceSource(ctx, op, state)
	if err != nil {
		return zero, err
	}
	if receipt, ok := state.Evidence[op.ID+"/"+t.AttemptID]; !ok || receipt != *t.Receipt {
		return zero, conflict("HANDOFF_EVIDENCE_UNVERIFIED", "Published evidence receipt changed")
	}
	living, err := s.captureRetirementFile(ctx, shared, doc, p.SubmittedHash, -1, domain.MaxReplacementHandoffBytes)
	if err != nil {
		return zero, err
	}
	r, err := s.openRoot(shared)
	if err != nil {
		return zero, err
	}
	f, err := openFile(r, doc)
	if err != nil {
		_ = r.Close()
		return zero, err
	}
	handoff, readErr := io.ReadAll(io.LimitReader(f, domain.MaxReplacementHandoffBytes+1))
	closeErr := f.Close()
	_ = r.Close()
	if readErr != nil {
		return zero, readErr
	}
	if closeErr != nil {
		return zero, closeErr
	}
	if hash(handoff) != p.SubmittedHash {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Frozen handoff changed")
	}
	if err := living.check(s); err != nil {
		return zero, err
	}
	in := EvidenceInput{Actor: Actor{SessionID: op.SourceSessionID}, Lease: Grant{Epoch: p.LeaseEpoch}, OperationID: op.ID, AttemptID: t.AttemptID, HandoffPath: doc, HandoffHash: p.SubmittedHash, Export: out, CreatedAt: *t.ExportedAt}
	bundle, err := s.buildEvidenceBundle(op.ProjectID, in, state, shared, handoff)
	if err != nil {
		return zero, err
	}
	if bundle.receipt != *t.Receipt || hash(bundle.transcript) != t.TranscriptHash {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Retirement evidence differs from the published bundle")
	}
	files := []retirementFileWitness{living}
	for _, entry := range []struct {
		name  string
		data  []byte
		limit int64
	}{{"handoff.md", handoff, domain.MaxReplacementHandoffBytes}, {"transcript.md", bundle.transcript, 512 << 20}, {"manifest.json", bundle.manifest, 256 << 20}} {
		witness, err := s.captureRetirementFile(ctx, bundle.receipt.Directory, entry.name, hash(entry.data), int64(len(entry.data)), entry.limit)
		if err != nil {
			return zero, err
		}
		files = append(files, witness)
	}
	native, err := out.RevalidateForPublication(ctx)
	if err != nil {
		return zero, err
	}
	proof := ReplacementRetirementProof{operation: op.ID, revision: op.Revision, source: source, documentRevision: state.Revision}
	proof.check = func(reserved context.Context) error {
		if err := reserved.Err(); err != nil {
			return err
		}
		if err := native.Check(reserved); err != nil {
			return err
		}
		for _, file := range files {
			if err := file.check(s); err != nil {
				return err
			}
		}
		_, currentRoot, currentDoc, err := s.state(reserved, op.ProjectID)
		if err != nil {
			return err
		}
		if currentRoot != shared || currentDoc != doc {
			return conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Configured handoff location changed")
		}
		snapshot, err := s.store.LoadTranscriptSourceSnapshot(reserved, source.ID)
		if err != nil {
			return err
		}
		if snapshot.Fingerprint != t.SnapshotFingerprint || snapshot.Session.ControllerOwner() != op.SourceOwner {
			return conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Source transcript snapshot changed before retirement")
		}
		return nil
	}
	if err := proof.Check(ctx); err != nil {
		return zero, err
	}
	return proof, nil
}

type retirementFileWitness struct {
	root, path  string
	rootInfo    os.FileInfo
	fingerprint filefingerprint.Fingerprint
}

func (s *Service) captureRetirementFile(ctx context.Context, root, path, want string, size, limit int64) (retirementFileWitness, error) {
	var zero retirementFileWitness
	r, err := s.openRoot(root)
	if err != nil {
		return zero, err
	}
	defer func() { _ = r.Close() }()
	rootInfo, err := r.Stat(".")
	if err != nil {
		return zero, err
	}
	f, err := openFile(r, path)
	if err != nil {
		return zero, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return zero, err
	}
	fingerprint, ok := filefingerprint.Capture(info)
	if !ok {
		return zero, conflict("HANDOFF_EVIDENCE_STABILITY_UNSUPPORTED", "Native file change-time verification is unavailable")
	}
	if err := verifyEvidence(ctx, f, want, size, limit); err != nil {
		return zero, err
	}
	w := retirementFileWitness{root: root, path: path, rootInfo: rootInfo, fingerprint: fingerprint}
	if err := w.check(s); err != nil {
		return zero, err
	}
	return w, nil
}

func (w retirementFileWitness) check(s *Service) error {
	r, err := s.openRoot(w.root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	info, err := r.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(w.rootInfo, info) {
		return conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Evidence root identity changed")
	}
	f, err := openFile(r, w.path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	named, err := r.Lstat(w.path)
	if err != nil {
		return err
	}
	if !w.fingerprint.Matches(opened) || !w.fingerprint.Matches(named) {
		return conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Verified evidence file changed")
	}
	return nil
}
