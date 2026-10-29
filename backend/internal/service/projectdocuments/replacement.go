package projectdocuments

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type replacementStore interface {
	CaptureTranscriptSourceCutoff(context.Context, domain.SessionID) (domain.TranscriptSourceCutoff, error)
	GetOrchestratorReplacement(context.Context, string) (domain.OrchestratorReplacement, bool, error)
	SaveOrchestratorReplacement(context.Context, domain.OrchestratorReplacement) (bool, error)
	WithReplacementDocumentPublication(context.Context, string, int64, domain.SessionRecord, int64, func(context.Context) error) (bool, error)
}

// ReplacementPreparationInput authenticates a source's receipt submission. It
// is never persisted verbatim: Actor.Capability exists only for this request.
type ReplacementPreparationInput struct {
	Actor          Actor  `json:"actor"`
	AttemptID      string `json:"attemptId"`
	Nonce          string `json:"nonce"`
	Path           string `json:"path"`
	Hash           string `json:"hash"`
	WriteReceiptID string `json:"writeReceiptId"`
}

// RecordReplacementPreparation validates actual bytes and author identity, then
// records submission without asserting that the provider turn has settled.
func (s *Service) RecordReplacementPreparation(ctx context.Context, id string, revision int64, in ReplacementPreparationInput, validate func(File) error) (domain.OrchestratorReplacement, error) {
	var zero domain.OrchestratorReplacement
	store, ok := s.store.(replacementStore)
	if !ok {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_UNAVAILABLE", "Replacement document store is unavailable")
	}
	op, found, err := store.GetOrchestratorReplacement(ctx, id)
	if err != nil {
		return zero, err
	}
	if !found || op.Revision != revision || op.Preparation == nil || op.Phase != domain.ReplacementPreparingHandoff {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Preparation operation changed")
	}
	p := op.Preparation
	if validate == nil || in.Actor.SessionID != op.SourceSessionID || in.AttemptID != p.AttemptID || in.Nonce != p.Nonce || in.Path != op.DocumentPath || in.Hash == "" || in.Hash == p.BaselineHash || in.WriteReceiptID == "" || in.WriteReceiptID == p.BaselineWriteReceiptID {
		return zero, conflict("ORCHESTRATOR_HANDOFF_RECEIPT_INVALID", "Receipt is not bound to this source preparation")
	}
	unlock := s.lock(op.ProjectID)
	defer unlock()
	source, err := s.authorize(ctx, op.ProjectID, in.Actor)
	if err != nil {
		return zero, err
	}
	if source.ControllerOwner() != op.SourceOwner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_SOURCE_CHANGED", "Preparation owner changed")
	}
	state, root, doc, err := s.state(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	if root != op.SharedDir || doc != op.DocumentPath {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_CONFIG_CHANGED", "Preparation location changed")
	}
	lease, r := state.Lease, state.LastHandoffWrite
	if lease == nil || lease.Frozen || lease.TokenHash == "" || lease.ReplacementID != op.ID || lease.Epoch != p.LeaseEpoch || lease.Owner != op.SourceOwner || r == nil || r.ID != in.WriteReceiptID || r.Path != doc || r.Hash != in.Hash || r.SourceSessionID != source.ID || r.Owner != op.SourceOwner || r.LeaseEpoch != p.LeaseEpoch || r.CapabilityVerifier != op.SourceCapabilityVerifier {
		return zero, conflict("ORCHESTRATOR_HANDOFF_RECEIPT_INVALID", "Latest handoff write does not match submitted receipt")
	}
	file, err := s.replacementHandoffFile(root, doc)
	if err != nil {
		return zero, err
	}
	if file.Hash != in.Hash {
		return zero, conflict("ORCHESTRATOR_HANDOFF_SOURCE_CHANGED", "Submitted handoff bytes changed")
	}
	if err := validate(file); err != nil {
		return zero, err
	}
	if p.SubmittedAt != nil && p.SubmittedHash == in.Hash && p.SubmittedPath == in.Path && p.WriteReceiptID == in.WriteReceiptID {
		return op, nil
	}
	now := time.Now().UTC()
	p.SubmittedHash, p.SubmittedPath, p.WriteReceiptID, p.SubmittedAt, p.VerifiedAt = in.Hash, in.Path, in.WriteReceiptID, &now, nil
	op.UpdatedAt = now
	published, err := store.WithReplacementDocumentPublication(ctx, op.ID, op.Revision, source, state.Revision, func(reserved context.Context) error {
		actual, err := s.replacementHandoffFile(root, doc)
		if err != nil {
			return err
		}
		if actual.Hash != in.Hash {
			return conflict("ORCHESTRATOR_HANDOFF_SOURCE_CHANGED", "Submitted handoff changed before acceptance")
		}
		ok, err := store.SaveOrchestratorReplacement(reserved, op)
		if err != nil {
			return err
		}
		if !ok {
			return conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Preparation revision changed")
		}
		return nil
	})
	if err != nil {
		return zero, err
	}
	if !published {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Preparation owner or revision changed")
	}
	op.Revision++
	return op, nil
}

func (s *Service) replacementHandoffFile(root, p string) (File, error) {
	r, err := s.openRoot(root)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = r.Close() }()
	b, err := readFile(r, p)
	if err != nil {
		return File{}, err
	}
	if len(b) > domain.MaxReplacementHandoffBytes || !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
		return File{}, conflict("ORCHESTRATOR_HANDOFF_CONTENT_INVALID", "A bounded UTF-8 handoff is required")
	}
	return fileView(p, b), nil
}

// FreezeReplacementHandoff verifies the author's receipt against the actual
// file and atomically records the immutable publication boundary.
func (s *Service) FreezeReplacementHandoff(ctx context.Context, id string, revision int64) (domain.OrchestratorReplacement, error) {
	var zero domain.OrchestratorReplacement
	store, ok := s.store.(replacementStore)
	if !ok {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_UNAVAILABLE", "Replacement document store is unavailable")
	}
	op, found, err := store.GetOrchestratorReplacement(ctx, id)
	if err != nil {
		return zero, err
	}
	if !found || op.Revision != revision || op.Preparation == nil {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Preparation operation changed")
	}
	unlock := s.lock(op.ProjectID)
	defer unlock()
	state, root, doc, err := s.state(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	p := op.Preparation
	if root != op.SharedDir || doc != op.DocumentPath || p.VerifiedAt == nil || p.SubmittedHash == "" || p.SubmittedPath != doc || p.WriteReceiptID == "" || p.WriteReceiptID == p.BaselineWriteReceiptID || p.SubmittedHash == p.BaselineHash {
		return zero, conflict("ORCHESTRATOR_HANDOFF_UNVERIFIED", "Current verified handoff is required")
	}
	lease := state.Lease
	if lease == nil || lease.ReplacementID != op.ID || lease.SessionID != op.SourceSessionID || lease.Owner != op.SourceOwner || lease.Epoch != p.LeaseEpoch || lease.TokenHash == "" {
		return zero, conflict("ORCHESTRATOR_HANDOFF_LEASE_CHANGED", "Preparation writer lease changed")
	}
	if op.Phase == domain.ReplacementExportingTranscript && lease.Frozen && op.FrozenAt != nil {
		return op, nil
	}
	if op.Phase != domain.ReplacementFreezingSharedWrites || lease.Frozen {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_PHASE_INVALID", "Operation cannot freeze handoff")
	}
	r := state.LastHandoffWrite
	if r == nil || r.ID != p.WriteReceiptID || r.Hash != p.SubmittedHash || r.Path != doc || r.SourceSessionID != op.SourceSessionID || r.Owner != op.SourceOwner || r.LeaseEpoch != p.LeaseEpoch || r.CapabilityVerifier != op.SourceCapabilityVerifier {
		return zero, conflict("ORCHESTRATOR_HANDOFF_RECEIPT_INVALID", "Handoff has no matching managed author receipt")
	}
	if state.Adoption != nil && !state.Adoption.Complete {
		return zero, conflict("PROJECT_DOCUMENT_ADOPTION_PENDING", "Living file adoption must finish before freeze")
	}
	source, found, err := s.store.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		return zero, err
	}
	if !found {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_SOURCE_CHANGED", "Source no longer exists")
	}
	checkFile := func() error {
		file, err := s.replacementHandoffFile(root, doc)
		if err != nil {
			return err
		}
		if file.Hash != p.SubmittedHash {
			return conflict("ORCHESTRATOR_HANDOFF_SOURCE_CHANGED", "Verified handoff bytes changed")
		}
		return nil
	}
	if err := checkFile(); err != nil {
		return zero, err
	}
	now := time.Now().UTC()
	op.FrozenAt, op.Phase, op.UpdatedAt = &now, domain.ReplacementExportingTranscript, now
	published, err := store.WithReplacementDocumentPublication(ctx, op.ID, op.Revision, source, state.Revision, func(reserved context.Context) error {
		current, found, err := store.GetOrchestratorReplacement(reserved, op.ID)
		if err != nil {
			return err
		}
		if !found || current.IntakeRevision != p.IntakeRevision {
			return conflict("ORCHESTRATOR_HANDOFF_INTAKE_CHANGED", "New instructions require a fresh handoff preparation")
		}
		sessions, err := s.store.ListSessions(reserved, op.ProjectID)
		if err != nil {
			return err
		}
		for _, session := range sessions {
			if session.Kind == domain.KindWorker && !session.IsTerminated && !slices.Contains(p.ExpectedSessions, session.ID) {
				return conflict("ORCHESTRATOR_HANDOFF_SESSION_MAP_CHANGED", "A new worker requires an updated section B before freeze")
			}
		}
		if err := checkFile(); err != nil {
			return err
		}
		cut, err := store.CaptureTranscriptSourceCutoff(reserved, op.SourceSessionID)
		if err != nil {
			return err
		}
		op.Transcript = &domain.ReplacementTranscript{AttemptID: uuid.NewString(), AO: cut}
		state.Lease.Frozen = true
		if err := s.save(reserved, op.ProjectID, &state); err != nil {
			return err
		}
		ok, err := store.SaveOrchestratorReplacement(reserved, op)
		if err != nil {
			return err
		}
		if !ok {
			return conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Freeze operation revision changed")
		}
		return nil
	})
	if err != nil {
		return zero, err
	}
	if !published {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Freeze owner or revision changed")
	}
	op.Revision++
	return op, nil
}

// BeginReplacementPreparation establishes one operation-owned claimable source
// lease and binds the current baseline before its standard prompt is delivered.
func (s *Service) BeginReplacementPreparation(ctx context.Context, id string, revision int64) (domain.OrchestratorReplacement, error) {
	var zero domain.OrchestratorReplacement
	store, ok := s.store.(replacementStore)
	if !ok {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_UNAVAILABLE", "Replacement document store is unavailable")
	}
	op, found, err := store.GetOrchestratorReplacement(ctx, id)
	if err != nil {
		return zero, err
	}
	if !found || op.Revision != revision || op.Preparation == nil {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Preparation operation changed")
	}
	unlock := s.lock(op.ProjectID)
	defer unlock()
	state, root, doc, err := s.state(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	if root != op.SharedDir || doc != op.DocumentPath {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_CONFIG_CHANGED", "Configured handoff location changed")
	}
	source, found, err := s.store.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		return zero, err
	}
	if !found || source.IsTerminated || source.ControllerOwner() != op.SourceOwner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier || op.SourceCapabilityVerifier == "" {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_SOURCE_CHANGED", "Source ownership changed")
	}
	if source.Mode == domain.SessionModeChat && source.Metadata.ControllerGeneration == "" || source.Mode != domain.SessionModeChat && source.Metadata.RuntimeLaunchID == "" {
		return zero, conflict("PROJECT_DOCUMENT_CONTROLLER_UNAVAILABLE", "Source controller is not ready")
	}
	if op.Phase == domain.ReplacementPreparingHandoff && op.Preparation.LeaseEpoch > 0 {
		if state.Lease == nil || state.Lease.ReplacementID != op.ID || state.Lease.SessionID != source.ID || state.Lease.Owner != source.ControllerOwner() || state.Lease.Epoch != op.Preparation.LeaseEpoch || state.Lease.Frozen {
			return zero, conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Prepared writer lease changed")
		}
		return op, nil
	}
	if op.Phase != domain.ReplacementAdoptingSharedFiles || op.Preparation.ControlTurnID != "" || op.Preparation.AttemptID == "" || op.Preparation.Nonce == "" {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_PHASE_INVALID", "Operation cannot begin document preparation")
	}
	if err := evidenceIDs(op.ID, op.Preparation.AttemptID); err != nil {
		return zero, err
	}
	epoch := int64(1)
	if old := state.Lease; old != nil {
		if old.Frozen {
			return zero, conflict("PROJECT_DOCUMENT_LEASE_FROZEN", "Project documents are frozen")
		}
		if old.SessionID != source.ID {
			previous, found, err := s.store.GetSession(ctx, old.SessionID)
			if err != nil {
				return zero, err
			}
			if !found || !previous.IsTerminated {
				return zero, conflict("PROJECT_DOCUMENT_LEASE_HELD", "Previous writer has not retired")
			}
		}
		epoch = old.Epoch + 1
	}
	baseline := MissingHash
	if state.Adoption == nil || state.Adoption.Complete {
		r, err := s.openRoot(root)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return zero, err
		}
		if r != nil {
			b, readErr := readFile(r, doc)
			_ = r.Close()
			if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
				return zero, readErr
			}
			if readErr == nil {
				baseline = hash(b)
			}
		}
	}
	op.Preparation.LeaseEpoch, op.Preparation.BaselineHash = epoch, baseline
	op.Preparation.ScratchDir = filepath.Join(s.dataDir, "projects", projectKey(op.ProjectID), "orchestrator", "preparations", op.ID, op.Preparation.AttemptID)
	if receipt := state.LastHandoffWrite; receipt != nil && receipt.Path == doc && receipt.Hash == baseline {
		op.Preparation.BaselineWriteReceiptID = receipt.ID
	}
	op.Phase, op.UpdatedAt = domain.ReplacementPreparingHandoff, time.Now().UTC()
	published, err := store.WithReplacementDocumentPublication(ctx, op.ID, op.Revision, source, state.Revision, func(reserved context.Context) error {
		if err := s.ensureRoot(root); err != nil {
			return err
		}
		if err := s.ensureRoot(op.Preparation.ScratchDir); err != nil {
			return err
		}
		current, found, err := store.GetOrchestratorReplacement(reserved, op.ID)
		if err != nil {
			return err
		}
		if !found {
			return conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Preparation operation disappeared")
		}
		op.IntakeRevision, op.Preparation.IntakeRevision = current.IntakeRevision, current.IntakeRevision
		state.Lease = &domain.ProjectDocumentLease{ReplacementID: op.ID, ClaimVerifier: op.SourceCapabilityVerifier, SessionID: source.ID, Owner: source.ControllerOwner(), Epoch: epoch}
		if err := s.save(reserved, op.ProjectID, &state); err != nil {
			return err
		}
		ok, err := store.SaveOrchestratorReplacement(reserved, op)
		if err != nil {
			return err
		}
		if !ok {
			return conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Preparation revision changed")
		}
		return nil
	})
	if err != nil {
		return zero, err
	}
	if !published {
		return zero, conflict("ORCHESTRATOR_REPLACEMENT_CHANGED", "Preparation owner or revision changed")
	}
	op.Revision++
	return op, nil
}
