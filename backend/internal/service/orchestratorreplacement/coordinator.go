package orchestratorreplacement

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

var errRevisionChanged = errors.New("replacement revision changed")

// Advance reconciles one durable boundary. It can be called again after caller
// loss or daemon restart; no phase alone proves a provider action completed.
func (s *Service) Advance(ctx context.Context, id string) (result domain.OrchestratorReplacement, resultErr error) {
	defer func() {
		if recover() != nil {
			result, resultErr = s.suspendAfterPanic(ctx, id)
		}
	}()
	op, err := s.Get(ctx, id)
	if err != nil {
		return op, err
	}
	run := func(ctx context.Context) error {
		op, err = s.Get(ctx, id)
		if err != nil || op.Phase.Terminal() {
			return err
		}
		switch op.Phase {
		case domain.ReplacementAwaitingSource, domain.ReplacementAwaitingHandoff, domain.ReplacementAwaitingTranscript:
			op, err = s.releaseSourceIntake(ctx, op)
			return err
		case domain.ReplacementRecoveryRequired:
			return nil
		}
		if op.SourceRetiredAt == nil {
			if err = s.checkSource(ctx, op); err != nil {
				op, err = s.wait(ctx, op, domain.ReplacementAwaitingSource, op.Phase, err)
				if errors.Is(err, domain.ErrReplacementRepairCapacity) {
					op, err = s.pauseForCapacity(ctx, op, err)
				}
				return err
			}
		}
		next, stepErr := s.advance(ctx, op)
		err = stepErr
		// A step may commit release before a later receipt fails. Carry its
		// returned revision into wait's committed-row/CAS revalidation; a
		// failed save at the old revision still cannot persist mutated facts.
		if err == nil || next.ID == op.ID && next.Revision > op.Revision {
			op = next
		}
		if errors.Is(err, domain.ErrReplacementRepairCapacity) {
			// A refused intent was never committed. Pause the unchanged live
			// source using recovery capacity so ordinary input can continue.
			op, err = s.pauseForCapacity(ctx, op, stepErr)
			return err
		}
		if errors.Is(err, errNoStopClearPending) {
			// Quiesce positively reported no stop attempt. Keep the durable
			// export intent reconcilable if its clearance could not commit.
			return err
		}
		if errors.Is(err, errRevisionChanged) {
			op, err = s.Get(ctx, id)
			return err
		}
		if err != nil {
			phase := domain.ReplacementAwaitingHandoff
			if op.SourceRetiredAt != nil {
				phase = domain.ReplacementRecoveryRequired
			} else if op.Phase == domain.ReplacementWaitingSourceIdle || errors.Is(err, domain.ErrReplacementSourceChanged) {
				phase = domain.ReplacementAwaitingSource
			} else if op.Phase == domain.ReplacementExportingTranscript || op.Phase == domain.ReplacementTranscriptVerified || op.Phase == domain.ReplacementRetiring {
				phase = domain.ReplacementAwaitingTranscript
				if sourceStopRecoveryRequired(err) {
					phase = domain.ReplacementRecoveryRequired
				}
			}
			op, err = s.wait(ctx, op, phase, op.Phase, err)
		}
		return err
	}
	if s.deps.WithProjectGate != nil {
		err = s.deps.WithProjectGate(ctx, op.ProjectID, run)
	} else {
		func() {
			unlock := s.lockProject(op.ProjectID)
			defer unlock()
			err = run(ctx)
		}()
	}
	if err == nil && op.Phase == domain.ReplacementNotifying && op.IntakeDrainRequestedAt != nil && len(op.Notifications) > 0 {
		return s.reconcileNotifications(ctx, op)
	}
	return op, err
}

func (s *Service) suspendAfterPanic(ctx context.Context, id string) (result domain.OrchestratorReplacement, resultErr error) {
	cause := apierr.Conflict("ORCHESTRATOR_REPLACEMENT_PANIC", "Replacement step panicked; retained evidence requires operator inspection before retry", nil)
	// A broken dependency may also panic during quarantine. Keep that from
	// crashing the daemon; no successful suspension is claimed without a save.
	defer func() {
		if recover() != nil {
			resultErr = cause
		}
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	current, err := s.Get(ctx, id)
	if err != nil {
		return current, errors.Join(cause, err)
	}
	result = current
	run := func(ctx context.Context) error {
		latest, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		result = latest
		if latest.Phase.Terminal() || latest.Phase == domain.ReplacementRecoveryRequired {
			return nil
		}
		result, err = s.wait(ctx, latest, domain.ReplacementRecoveryRequired, latest.Phase, cause)
		return err
	}
	if s.deps.WithProjectGate != nil {
		err = s.deps.WithProjectGate(ctx, current.ProjectID, run)
	} else {
		unlock := s.lockProject(current.ProjectID)
		defer unlock()
		err = run(ctx)
	}
	return result, errors.Join(cause, err)
}

func (s *Service) lockProject(id domain.ProjectID) func() {
	s.locksMu.Lock()
	if s.locks == nil {
		s.locks = make(map[domain.ProjectID]*sync.Mutex)
	}
	mu := s.locks[id]
	if mu == nil {
		mu = &sync.Mutex{}
		s.locks[id] = mu
	}
	s.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (s *Service) checkProject(ctx context.Context, op domain.OrchestratorReplacement) error {
	p, found, err := s.deps.Store.GetProject(ctx, string(op.ProjectID))
	if err != nil {
		return err
	}
	if !found || !p.ArchivedAt.IsZero() {
		return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_CONFIG_CHANGED", "Project is no longer active", nil)
	}
	fingerprint, err := domain.ReplacementProjectFingerprint(p)
	if err != nil {
		return err
	}
	if fingerprint != op.ProjectFingerprint {
		return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_CONFIG_CHANGED", "Project settings changed after acceptance", nil)
	}
	return nil
}

func (s *Service) checkSource(ctx context.Context, op domain.OrchestratorReplacement) error {
	if err := s.checkProject(ctx, op); err != nil {
		return err
	}
	source, found, err := s.deps.Store.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		return err
	}
	if !found || source.IsTerminated || source.ControllerOwner() != op.SourceOwner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_SOURCE_CHANGED", "Source owner changed after acceptance", nil)
	}
	return nil
}

func (s *Service) save(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	op.UpdatedAt = s.deps.Now().UTC()
	var changed bool
	var err error
	if op.SourceStop != nil {
		changed, err = s.deps.Store.SaveReplacementRuntimeProgress(ctx, op)
	} else {
		changed, err = s.deps.Store.SaveOrchestratorReplacement(ctx, op)
	}
	if err != nil {
		return op, checkpointCapacityError(err)
	}
	if !changed {
		return op, errRevisionChanged
	}
	op.Revision++
	return op, nil
}

func checkpointCapacityError(err error) error {
	if !errors.Is(err, domain.ErrReplacementRepairCapacity) {
		return err
	}
	var api *apierr.Error
	if errors.As(err, &api) && api.Code == "ORCHESTRATOR_REPLACEMENT_REPAIR_CAPACITY" {
		return err
	}
	return errors.Join(apierr.Conflict("ORCHESTRATOR_REPLACEMENT_REPAIR_CAPACITY", "Replacement checkpoint capacity is exhausted; inspect the operation's committed recovery facts", nil), err)
}

func (s *Service) pauseForCapacity(ctx context.Context, op domain.OrchestratorReplacement, cause error) (domain.OrchestratorReplacement, error) {
	// Advance attempts this once, after an ordinary write was refused.
	// Always read committed facts; wait itself never retries a write.
	current, err := s.Get(ctx, op.ID)
	if err != nil {
		return op, err
	}
	paused := current
	paused.Phase = domain.ReplacementAwaitingSource
	if !paused.CanReleaseSourceIntake() {
		return current, checkpointCapacityError(cause)
	}
	return s.wait(ctx, current, paused.Phase, current.Phase, checkpointCapacityError(cause))
}

func (s *Service) wait(ctx context.Context, op domain.OrchestratorReplacement, phase, resume domain.ReplacementPhase, cause error) (domain.OrchestratorReplacement, error) {
	// advance may have mutated facts before a failed save. Only committed facts
	// may enter a wait/recovery receipt; never persist that uncommitted copy.
	current, err := s.Get(ctx, op.ID)
	if err != nil {
		return op, err
	}
	if current.Revision != op.Revision {
		return current, errRevisionChanged
	}
	op = current
	if op.SourceStopIntentAt != nil || op.SourceStop != nil || op.SourceRetiredAt != nil {
		switch phase {
		case domain.ReplacementAwaitingSource, domain.ReplacementAwaitingHandoff, domain.ReplacementAwaitingTranscript:
			phase = domain.ReplacementRecoveryRequired
		}
	}
	failure := domain.ReplacementFailure{Code: "ORCHESTRATOR_REPLACEMENT_STEP_FAILED", Message: "Replacement step could not be verified", Retryable: true}
	var api *apierr.Error
	var native *ports.NativeTranscriptError
	if errors.As(cause, &api) {
		failure.Code, failure.Message = api.Code, api.Message
	} else if errors.As(cause, &native) {
		failure.Code, failure.Message = native.Code, native.Reason
	} else if errors.Is(cause, domain.ErrReplacementSourceChanged) {
		failure.Code, failure.Message = "ORCHESTRATOR_REPLACEMENT_SOURCE_CHANGED", "Source owner changed during the replacement action"
	} else if errors.Is(cause, domain.ErrReplacementConfigChanged) {
		failure.Code, failure.Message = "ORCHESTRATOR_REPLACEMENT_CONFIG_CHANGED", "Pinned project or target settings changed during replacement"
	} else if errors.Is(cause, domain.ErrReplacementSourceBusy) {
		failure.Code, failure.Message = "ORCHESTRATOR_HANDOFF_SOURCE_BUSY", "Source is not safely quiescent; no stop was attempted"
	} else if errors.Is(cause, domain.ErrReplacementIntakeUncertain) {
		failure.Code, failure.Message = "ORCHESTRATOR_HANDOFF_INTAKE_UNCERTAIN", "Pending input may already have reached the provider; verify it before retrying"
	} else if errors.Is(cause, domain.ErrReplacementIntakeConflict) {
		failure.Code, failure.Message = "ORCHESTRATOR_HANDOFF_INTAKE_CONFLICT", "Pending input or edit receipts conflict with the successor queue"
	} else if errors.Is(cause, ports.ErrChatRecoveryInconclusive) {
		failure.Code, failure.Message = "ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN", "Source shutdown cannot be positively confirmed for the current owner"
		if op.SourceRetiredAt != nil {
			failure.Code, failure.Message = "ORCHESTRATOR_HANDOFF_SUCCESSOR_STOP_UNCERTAIN", "Successor shutdown cannot be positively confirmed for the current owner"
		}
	}
	if retry := op.RetainedRetry; retry != nil && retry.Phase == op.Phase && retry.Failure == failure {
		fingerprint, err := domain.ReplacementReconciliationHash(op)
		if err != nil {
			return op, err
		}
		if fingerprint == retry.FactsSHA256 {
			return op, nil
		}
	}
	if op.Phase == phase && op.ResumePhase == resume && op.Error != nil && *op.Error == failure {
		return s.releaseSourceIntake(ctx, op)
	}
	op.Phase, op.ResumePhase, op.Error = phase, resume, &failure
	op, err = s.save(ctx, op)
	if err != nil {
		if errors.Is(err, domain.ErrReplacementRepairCapacity) && cause != nil {
			return current, errors.Join(apierr.Conflict("ORCHESTRATOR_REPLACEMENT_REPAIR_CAPACITY", "Replacement checkpoint capacity is exhausted; original failure: "+cause.Error(), nil), err, cause)
		}
		return current, err
	}
	return s.releaseSourceIntake(ctx, op)
}

func sourceStopRecoveryRequired(err error) bool {
	var api *apierr.Error
	if errors.As(err, &api) {
		return api.Code == "ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN" || api.Code == "ORCHESTRATOR_HANDOFF_RETIREMENT_UNVERIFIED"
	}
	var native *ports.NativeTranscriptError
	if errors.As(err, &native) {
		return native.Code == "ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN"
	}
	return errors.Is(err, ports.ErrChatRecoveryInconclusive)
}

func (s *Service) advance(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	switch op.Phase {
	case domain.ReplacementAccepted:
		op.Phase = domain.ReplacementPreflight
		return s.save(ctx, op)
	case domain.ReplacementPreflight:
		snapshot, err := s.deps.Store.LoadTranscriptSourceSnapshot(ctx, op.SourceSessionID)
		if err != nil {
			return op, err
		}
		if _, err := transcriptexport.RequiredNativeEpochs(snapshot); err != nil {
			return s.wait(ctx, op, domain.ReplacementAwaitingTranscript, op.Phase, err)
		}
		op.Phase, op.Error = domain.ReplacementWaitingSourceIdle, nil
		return s.save(ctx, op)
	case domain.ReplacementWaitingSourceIdle:
		if !s.deps.Now().Before(op.DeadlineAt) {
			return s.wait(ctx, op, domain.ReplacementAwaitingSource, op.Phase, apierr.Conflict("ORCHESTRATOR_HANDOFF_SOURCE_IDLE_TIMEOUT", "Source did not become safely idle before the deadline", nil))
		}
		observation, err := s.deps.Runtime.ObserveReplacementSource(ctx, op)
		if err != nil {
			if pendingSourceObservation(err) {
				return s.wait(ctx, op, op.Phase, op.Phase, err)
			}
			return op, err
		}
		if err := positiveIdle(op, observation); err != nil {
			return s.wait(ctx, op, domain.ReplacementWaitingSourceIdle, op.Phase, err)
		}
		sessions, err := s.deps.Store.ListSessions(ctx, op.ProjectID)
		if err != nil {
			return op, err
		}
		ids := make([]domain.SessionID, 0, len(sessions))
		for _, session := range sessions {
			if session.Kind == domain.KindWorker && !session.IsTerminated {
				ids = append(ids, session.ID)
			}
		}
		slices.Sort(ids)
		now := s.deps.Now().UTC()
		attempt := uuid.NewString()
		op.Preparation = &domain.ReplacementPreparation{AttemptID: attempt, Nonce: uuid.NewString(), ControlClientID: "replacement:" + op.ID + ":prepare:" + attempt, StartedAt: now, DeadlineAt: op.DeadlineAt, ExpectedSessions: ids}
		op.Phase, op.Error = domain.ReplacementAdoptingSharedFiles, nil
		return s.save(ctx, op)
	case domain.ReplacementAdoptingSharedFiles:
		next, err := s.deps.Documents.BeginReplacementPreparation(ctx, op.ID, op.Revision)
		if err != nil {
			return op, err
		}
		return next, nil
	case domain.ReplacementPreparingHandoff:
		return s.prepare(ctx, op)
	case domain.ReplacementHandoffVerified:
		op.Phase = domain.ReplacementFreezingSharedWrites
		return s.save(ctx, op)
	case domain.ReplacementExportingTranscript:
		return s.export(ctx, op)
	case domain.ReplacementTranscriptVerified:
		if err := s.preflightRetirement(ctx, op); err != nil {
			return op, err
		}
		op.Phase = domain.ReplacementRetiring
		return s.save(ctx, op)
	case domain.ReplacementRetiring:
		return s.retire(ctx, op)
	case domain.ReplacementStarting:
		return s.startSuccessor(ctx, op)
	case domain.ReplacementControllerReady:
		return s.bootstrap(ctx, op)
	case domain.ReplacementHandoffSettled:
		return s.beginResumeVerification(ctx, op)
	case domain.ReplacementVerifyingResume:
		return s.verifyResume(ctx, op)
	case domain.ReplacementActivatingSharedWriter:
		return s.activateWriter(ctx, op)
	case domain.ReplacementNotifying:
		// PR 4 owns worker notification delivery, while verified ordinary intake
		// can resume without manufacturing notification acceptance/read facts.
		return s.releaseIntake(ctx, op)
	case domain.ReplacementFreezingSharedWrites:
		next, err := s.deps.Documents.FreezeReplacementHandoff(ctx, op.ID, op.Revision)
		if err != nil {
			return op, err
		}
		return next, nil
	default:
		return op, apierr.Conflict("ORCHESTRATOR_REPLACEMENT_PHASE_UNAVAILABLE", "Replacement phase is not yet available", nil)
	}
}

// These pre-stop observations may become verifiable as hooks/native writes
// flush. Callers enforce the existing deadline before observing again. Unknown
// schema/provenance and every post-stop export failure keep their hard barrier.
func pendingSourceObservation(err error) bool {
	if errors.Is(err, domain.ErrReplacementSourceBusy) {
		return true
	}
	var native *ports.NativeTranscriptError
	if !errors.As(err, &native) {
		return false
	}
	switch native.Code {
	case "TRANSCRIPT_TAIL_UNSETTLED", "TRANSCRIPT_COVERAGE_INCOMPLETE", "TRANSCRIPT_CUTOFF_INVALID":
		return true
	}
	return false
}

func sourceNativeID(owner domain.SessionControllerOwner) string {
	if domain.NormalizeSessionMode(owner.Mode) == domain.SessionModeChat {
		return owner.ProviderConversationID
	}
	return owner.AgentSessionID
}

func positiveIdle(op domain.OrchestratorReplacement, observation SourceObservation) error {
	if observation.Owner != op.SourceOwner || observation.ObservedAt.IsZero() || observation.Provenance != domain.NativeThreadMain || observation.NativeID == "" || observation.NativeID != sourceNativeID(op.SourceOwner) {
		return apierr.Conflict("ORCHESTRATOR_HANDOFF_SOURCE_UNVERIFIED", "Source main controller identity is not verified", nil)
	}
	if observation.PendingApproval {
		return apierr.Conflict("ORCHESTRATOR_HANDOFF_APPROVAL_PENDING", "Source has a pending approval", nil)
	}
	if observation.State != ports.ChatControllerReady || observation.ActiveTurn != nil && !observation.ActiveTurn.State.Terminal() {
		return apierr.Conflict("ORCHESTRATOR_HANDOFF_SOURCE_BUSY", "Source controller is not positively idle", nil)
	}
	return nil
}
