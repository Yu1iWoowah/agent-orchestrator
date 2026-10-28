package projectdocuments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

// EvidenceInput is daemon-internal. It is not an API for supplying arbitrary
// native paths or a caller's assertion that a transcript is complete.
type EvidenceInput struct {
	Actor       Actor
	Lease       Grant
	OperationID string
	AttemptID   string
	HandoffPath string
	HandoffHash string
	Export      transcriptexport.Output
	CreatedAt   time.Time
}

// EvidenceManifest binds immutable files to the frozen source and operation.
type EvidenceManifest struct {
	transcriptexport.Manifest
	OperationID          string                        `json:"operationId"`
	AttemptID            string                        `json:"attemptId"`
	ExportedAt           time.Time                     `json:"exportedAt"`
	SharedDirectory      string                        `json:"sharedDirectory"`
	HandoffPath          string                        `json:"handoffPath"`
	LeaseEpoch           int64                         `json:"leaseEpoch"`
	AdoptionManifestHash string                        `json:"adoptionManifestHash,omitempty"`
	Files                []domain.ProjectDocumentEntry `json:"files"`
}

func evidenceIDs(operation, attempt string) error {
	for _, id := range []string{operation, attempt} {
		u, e := uuid.Parse(id)
		if e != nil || u.String() != id || u == uuid.Nil {
			return apierr.Invalid("HANDOFF_EVIDENCE_PATH_INVALID", "Canonical nonzero operation and attempt UUIDs are required", nil)
		}
	}
	return nil
}
func evidenceFile(p string) error {
	if p != "handoff.md" && p != "transcript.md" && p != "manifest.json" {
		return apierr.Invalid("HANDOFF_EVIDENCE_PATH_INVALID", "Unknown evidence file", nil)
	}
	return nil
}
func (s *Service) evidenceDirectory(id domain.ProjectID, op, attempt string) string {
	return filepath.Join(s.dataDir, "projects", projectKey(id), "orchestrator", "handoffs", op, attempt)
}

// PublishEvidence uses the same durable writer reservation as living documents,
// but requires a frozen lease. A failed receipt save is recoverable from the
// identical, already-renamed bundle; it never rewrites a successful attempt.
func (s *Service) PublishEvidence(ctx context.Context, id domain.ProjectID, in EvidenceInput) (domain.HandoffEvidenceReceipt, error) {
	var zero domain.HandoffEvidenceReceipt
	if e := evidenceIDs(in.OperationID, in.AttemptID); e != nil {
		return zero, e
	}
	if !in.Export.Valid() || in.Export.Manifest.ProjectID != id || in.Export.Manifest.SourceID != in.Actor.SessionID || in.CreatedAt.IsZero() {
		return zero, conflict("HANDOFF_EVIDENCE_UNVERIFIED", "A verified transcript and fixed export time are required")
	}
	if e := cleanPath(in.HandoffPath); e != nil {
		return zero, e
	}
	unlock := s.lock(id)
	defer unlock()
	state, shared, doc, err := s.state(ctx, id)
	if err != nil {
		return zero, err
	}
	rec, err := s.validate(ctx, id, in.Actor, in.Lease, state, true)
	if err != nil {
		return zero, err
	}
	if state.Lease == nil || !state.Lease.Frozen || doc != in.HandoffPath || in.HandoffHash == "" {
		return zero, conflict("HANDOFF_EVIDENCE_NOT_FROZEN", "The configured handoff must be frozen")
	}
	if state.Adoption != nil && !state.Adoption.Complete {
		return zero, conflict("PROJECT_DOCUMENT_ADOPTION_PENDING", "Living file adoption is incomplete")
	}
	root, err := s.openRoot(shared)
	if err != nil {
		return zero, err
	}
	handoff, err := readFile(root, doc)
	_ = root.Close()
	if err != nil {
		return zero, err
	}
	if hash(handoff) != in.HandoffHash {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "The frozen handoff hash changed")
	}
	transcript := []byte(fmt.Sprintf("Operation: %s\nAttempt: %s\nExport time: %s\nKnown message time range: %s — %s\n\n", in.OperationID, in.AttemptID, in.CreatedAt.UTC().Format(time.RFC3339Nano), in.Export.Manifest.TimeStart, in.Export.Manifest.TimeEnd))
	transcript = append(transcript, in.Export.Transcript...)
	manifest := EvidenceManifest{Manifest: in.Export.Manifest, OperationID: in.OperationID, AttemptID: in.AttemptID, ExportedAt: in.CreatedAt.UTC(), SharedDirectory: shared, HandoffPath: doc, LeaseEpoch: in.Lease.Epoch, Files: []domain.ProjectDocumentEntry{{Path: "handoff.md", Hash: hash(handoff), Size: int64(len(handoff))}, {Path: "transcript.md", Hash: hash(transcript), Size: int64(len(transcript))}}}
	if state.Adoption != nil {
		manifest.AdoptionManifestHash = state.Adoption.ManifestHash
	}
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return zero, err
	}
	mb = append(mb, '\n')
	if len(transcript) > 512<<20 || len(mb) > 256<<20 {
		return zero, conflict("HANDOFF_EVIDENCE_TOO_LARGE", "Evidence exceeds publication capacity")
	}
	receipt := domain.HandoffEvidenceReceipt{Directory: s.evidenceDirectory(id, in.OperationID, in.AttemptID), ManifestHash: hash(mb), InputHash: hash([]byte(in.HandoffHash + "\x00" + hash(mb) + "\x00" + hash(transcript)))}
	key := in.OperationID + "/" + in.AttemptID
	if prior, ok := state.Evidence[key]; ok && prior != receipt {
		return zero, conflict("HANDOFF_EVIDENCE_CONFLICT", "This attempt already has different evidence")
	}
	parent := filepath.Dir(receipt.Directory)
	if err := s.ensureRoot(parent); err != nil {
		return zero, err
	}
	r, err := s.openRoot(parent)
	if err != nil {
		return zero, err
	}
	defer func() { _ = r.Close() }()
	stage := ".ao-handoff-" + uuid.NewString()
	if err := r.Mkdir(stage, 0o700); err != nil {
		return zero, err
	}
	defer func() { _ = r.RemoveAll(stage) }()
	files := map[string][]byte{"handoff.md": handoff, "transcript.md": transcript, "manifest.json": mb}
	for _, p := range []string{"handoff.md", "transcript.md", "manifest.json"} {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		f, e := r.OpenFile(stage+"/"+p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e != nil {
			return zero, e
		}
		_, e = f.Write(files[p])
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return zero, e
		}
		if closeErr != nil {
			return zero, closeErr
		}
	}
	if err := syncEvidenceDir(r, stage); err != nil {
		return zero, err
	}
	// Strict native parsing must not hold the store's global writer reservation.
	// The coordinator owns intake fencing throughout Build and publication.
	native, err := in.Export.RevalidateForPublication(ctx)
	if err != nil {
		return zero, err
	}
	published, err := s.store.WithProjectDocumentPublication(ctx, id, rec, state.Revision, func(reserved context.Context) error {
		_, currentRoot, currentDoc, e := s.state(reserved, id)
		if e != nil {
			return e
		}
		if currentRoot != shared || currentDoc != doc {
			return conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Configured handoff location changed")
		}
		snapshot, e := s.store.LoadTranscriptSourceSnapshot(reserved, rec.ID)
		if e != nil {
			return e
		}
		if snapshot.Fingerprint != manifest.SnapshotFingerprint || snapshot.Session.ControllerOwner() != manifest.Owner {
			return conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Source owner or transcript snapshot changed")
		}
		current, e := s.store.GetProjectDocumentState(reserved, id)
		if e != nil {
			return e
		}
		if current.Lease == nil || !current.Lease.Frozen || current.Lease.Epoch != in.Lease.Epoch {
			return conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Frozen lease changed")
		}
		// Recheck shared bytes inside the reservation, immediately before rename.
		sr, e := s.openRoot(shared)
		if e != nil {
			return e
		}
		b, e := readFile(sr, doc)
		_ = sr.Close()
		if e != nil {
			return e
		}
		if hash(b) != in.HandoffHash {
			return conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Frozen handoff changed")
		}
		if e := native.Check(reserved); e != nil {
			return e
		}
		info, e := r.Lstat(in.AttemptID)
		if e == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return conflict("HANDOFF_EVIDENCE_CONFLICT", "Evidence target is not an owned directory")
			}
			for _, p := range []string{"handoff.md", "transcript.md", "manifest.json"} {
				if e := compareEvidenceFile(ctx, r, in.AttemptID+"/"+p, hash(files[p]), int64(len(files[p]))); e != nil {
					return conflict("HANDOFF_EVIDENCE_CONFLICT", "Existing attempt bytes differ")
				}
			}
		} else if !errors.Is(e, fs.ErrNotExist) {
			return e
		} else {
			if e := ctx.Err(); e != nil {
				return e
			}
			if e := r.Rename(stage, in.AttemptID); e != nil {
				return e
			}
			if e := syncEvidenceDir(r, "."); e != nil {
				return e
			}
		}
		if current.Evidence == nil {
			current.Evidence = map[string]domain.HandoffEvidenceReceipt{}
		}
		if prior, ok := current.Evidence[key]; ok {
			if prior != receipt {
				return conflict("HANDOFF_EVIDENCE_CONFLICT", "Attempt receipt differs")
			}
			return nil
		}
		current.Evidence[key] = receipt
		ok, e := s.store.SaveProjectDocumentState(reserved, id, current)
		if e != nil {
			return e
		}
		if !ok {
			return conflict("HANDOFF_EVIDENCE_CONFLICT", "Receipt reservation changed")
		}
		return nil
	})
	if err != nil {
		return zero, err
	}
	if !published {
		return zero, conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Source authorization changed before publication")
	}
	return receipt, nil
}
func syncEvidenceDir(r *os.Root, p string) error {
	f, e := r.Open(p)
	if e != nil {
		return e
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// OpenEvidence verifies both the trusted manifest digest and the requested file
// before returning an owned descriptor. It never resolves paths under sessions.
func (s *Service) OpenEvidence(ctx context.Context, id domain.ProjectID, op, attempt, p string) (*os.File, error) {
	if e := evidenceIDs(op, attempt); e != nil {
		return nil, e
	}
	if e := evidenceFile(p); e != nil {
		return nil, e
	}
	state, _, _, e := s.state(ctx, id)
	if e != nil {
		return nil, e
	}
	receipt, ok := state.Evidence[op+"/"+attempt]
	if !ok {
		return nil, apierr.NotFound("HANDOFF_EVIDENCE_NOT_FOUND", "Published evidence not found")
	}
	expected := s.evidenceDirectory(id, op, attempt)
	if receipt.Directory != expected {
		return nil, conflict("HANDOFF_EVIDENCE_CORRUPT", "Evidence root identity changed")
	}
	r, e := s.openRoot(expected)
	if e != nil {
		return nil, conflict("HANDOFF_EVIDENCE_CORRUPT", "Published evidence is unavailable")
	}
	defer func() { _ = r.Close() }()
	mf, e := openEvidenceFile(r, "manifest.json")
	if e != nil {
		return nil, e
	}
	defer func() { _ = mf.Close() }()
	if e := verifyEvidence(ctx, mf, receipt.ManifestHash, -1, 256<<20); e != nil {
		return nil, e
	}
	var manifest EvidenceManifest
	if e := json.NewDecoder(io.LimitReader(mf, (256<<20)+1)).Decode(&manifest); e != nil {
		return nil, conflict("HANDOFF_EVIDENCE_CORRUPT", "Invalid evidence manifest")
	}
	if manifest.ProjectID != id || manifest.OperationID != op || manifest.AttemptID != attempt {
		return nil, conflict("HANDOFF_EVIDENCE_CORRUPT", "Evidence identity mismatch")
	}
	if p == "manifest.json" {
		_, e = mf.Seek(0, io.SeekStart)
		if e != nil {
			return nil, e
		}
		return duplicateEvidenceDescriptor(ctx, r, p, receipt.ManifestHash, -1, 256<<20)
	}
	for _, entry := range manifest.Files {
		if entry.Path == p {
			return duplicateEvidenceDescriptor(ctx, r, p, entry.Hash, entry.Size, 512<<20)
		}
	}
	return nil, conflict("HANDOFF_EVIDENCE_CORRUPT", "Requested file is absent from its manifest")
}

// ReadEvidence returns a bounded UTF-8 preview after verifying the receipt.
func (s *Service) ReadEvidence(ctx context.Context, id domain.ProjectID, op, attempt, p string) (File, error) {
	f, e := s.OpenEvidence(ctx, id, op, attempt, p)
	if e != nil {
		return File{}, e
	}
	defer func() { _ = f.Close() }()
	info, e := f.Stat()
	if e != nil {
		return File{}, e
	}
	if info.Size() > MaxFileBytes {
		return File{}, apierr.Invalid("HANDOFF_EVIDENCE_TOO_LARGE", "Use download for evidence larger than the preview limit", nil)
	}
	b, e := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if e != nil {
		return File{}, e
	}
	return fileView(p, b), nil
}
