package projectdocuments

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ActivateReplacementWriter moves the frozen lease after native verification.
func (s *Service) ActivateReplacementWriter(ctx context.Context, operation string, revision int64) (domain.OrchestratorReplacement, error) {
	var zero domain.OrchestratorReplacement
	store, ok := s.store.(replacementStore)
	if !ok {
		return zero, domain.ErrReplacementStateInvalid
	}
	op, found, err := store.GetOrchestratorReplacement(ctx, operation)
	if err != nil {
		return zero, err
	}
	if !found {
		return zero, domain.ErrReplacementStateInvalid
	}
	if op.WriterActivation != nil {
		return op, nil
	} // Lost replies never move the epoch twice.
	proof, verified := op.ExpectedSuccessorController()
	t, p := op.Transcript, op.Preparation
	if op.Revision != revision || op.Phase != domain.ReplacementActivatingSharedWriter || !verified || op.ResumeEvidence == nil || op.ResumeEvidence.VerifiedAt == nil || t == nil || t.Receipt == nil || p == nil || p.SubmittedHash == "" || t.TranscriptHash == "" {
		return zero, conflict("ORCHESTRATOR_HANDOFF_ACTIVATION_UNVERIFIED", "A verified main continuity probe is required")
	}
	if err := evidenceIDs(op.ID, t.AttemptID); err != nil {
		return zero, err
	}
	unlock := s.lock(op.ProjectID)
	defer unlock()
	state, shared, doc, err := s.state(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	lease := state.Lease
	if shared != op.SharedDir || doc != op.DocumentPath || lease == nil || !lease.Frozen || lease.ReplacementID != op.ID || lease.SessionID != op.SourceSessionID || lease.Owner != op.SourceOwner || lease.Epoch != p.LeaseEpoch || lease.Epoch <= 0 || lease.Epoch == math.MaxInt64 || lease.TokenHash == "" || state.Evidence[op.ID+"/"+t.AttemptID] != *t.Receipt || state.Adoption != nil && !state.Adoption.Complete {
		return zero, conflict("ORCHESTRATOR_HANDOFF_ACTIVATION_UNVERIFIED", "Frozen source lease or evidence changed")
	}
	dir := s.evidenceDirectory(op.ProjectID, op.ID, t.AttemptID)
	if dir != t.Receipt.Directory || hash([]byte(p.SubmittedHash+"\x00"+t.Receipt.ManifestHash+"\x00"+t.TranscriptHash)) != t.Receipt.InputHash {
		return zero, conflict("HANDOFF_EVIDENCE_UNVERIFIED", "Activation evidence receipt changed")
	}
	target, found, err := s.store.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		return zero, err
	}
	if !found || target.ProjectID != op.ProjectID || target.Kind != domain.KindOrchestrator || target.IsTerminated || target.ControllerOwner() != proof.Owner || target.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
		return zero, conflict("ORCHESTRATOR_HANDOFF_ACTIVATION_UNVERIFIED", "Resumed successor ownership changed")
	}
	var witnesses []retirementFileWitness
	for _, entry := range []struct {
		root, path, digest string
		limit              int64
	}{{shared, doc, p.SubmittedHash, domain.MaxReplacementHandoffBytes}, {dir, "handoff.md", p.SubmittedHash, domain.MaxReplacementHandoffBytes}, {dir, "transcript.md", t.TranscriptHash, 512 << 20}, {dir, "manifest.json", t.Receipt.ManifestHash, 256 << 20}} {
		witness, err := s.captureRetirementFile(ctx, entry.root, entry.path, entry.digest, -1, entry.limit)
		if err != nil {
			return zero, err
		}
		witnesses = append(witnesses, witness)
	}
	published, err := store.WithReplacementDocumentPublication(ctx, op.ID, op.Revision, target, state.Revision, func(reserved context.Context) error {
		for _, witness := range witnesses {
			if err := witness.check(s); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		workers, err := s.store.ListSessions(reserved, op.ProjectID)
		if err != nil {
			return err
		}
		slices.SortFunc(workers, func(a, b domain.SessionRecord) int {
			if a.ID < b.ID {
				return -1
			}
			if a.ID > b.ID {
				return 1
			}
			return 0
		})
		op.Notifications = []domain.ReplacementWorkerNotification{}
		for _, worker := range workers {
			if worker.Kind == domain.KindWorker && !worker.IsTerminated && !worker.IsTaskPreparation {
				op.Notifications = append(op.Notifications, domain.ReplacementWorkerNotification{SessionID: worker.ID, ContactID: "handoff-notice:" + op.ID + ":" + string(worker.ID), ObservedOwner: worker.ControllerOwner(), State: "pending", CreatedAt: now})
			}
		}
		op.WorkerFYIPayload = op.NewWorkerFYIPayload()
		op.WriterActivation = &domain.ReplacementWriterActivation{BeforeEpoch: lease.Epoch, AfterEpoch: lease.Epoch + 1, Owner: proof.Owner, CapabilityVerifier: proof.CapabilityVerifier, ActivatedAt: now}
		state.Lease = &domain.ProjectDocumentLease{ReplacementID: op.ID, SessionID: target.ID, Owner: proof.Owner, Epoch: lease.Epoch + 1, ClaimVerifier: proof.CapabilityVerifier, CapabilityVerifier: proof.CapabilityVerifier}
		if err := s.save(reserved, op.ProjectID, &state); err != nil {
			return err
		}
		op.Phase, op.ResumePhase, op.Error, op.UpdatedAt = domain.ReplacementNotifying, "", nil, now
		saved, err := store.SaveOrchestratorReplacement(reserved, op)
		if err != nil {
			return err
		}
		if !saved {
			return domain.ErrReplacementStateInvalid
		}
		return nil
	})
	if err != nil {
		return zero, err
	}
	if !published {
		return zero, conflict("ORCHESTRATOR_HANDOFF_ACTIVATION_UNVERIFIED", "Activation ownership or probe changed before the writer reservation")
	}
	op.Revision++
	return op, nil
}
