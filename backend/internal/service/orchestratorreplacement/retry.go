package orchestratorreplacement

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Retry preserves one operation, its evidence and its bound successor. Before
// source shutdown it may archive a failed attempt under a fresh preparation.
func (s *Service) Retry(ctx context.Context, id string) (domain.OrchestratorReplacement, error) {
	return s.resetBeforeStop(ctx, id, false)
}

// Cancel releases a pre-stop operation without stopping any controller.
func (s *Service) Cancel(ctx context.Context, id string) (domain.OrchestratorReplacement, error) {
	return s.resetBeforeStop(ctx, id, true)
}

func (s *Service) resetBeforeStop(ctx context.Context, id string, cancel bool) (domain.OrchestratorReplacement, error) {
	op, err := s.Get(ctx, id)
	if err != nil {
		return op, err
	}
	run := func(locked context.Context) error {
		var err error
		op, err = s.Get(locked, id)
		if err != nil {
			return err
		}
		if cancel && op.Phase == domain.ReplacementCancelled {
			return nil
		}
		if op.Phase.Terminal() {
			return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_TERMINAL", "The operation has already ended", nil)
		}
		waiting := op.Phase == domain.ReplacementAwaitingSource || op.Phase == domain.ReplacementAwaitingHandoff || op.Phase == domain.ReplacementAwaitingTranscript || op.Phase == domain.ReplacementRecoveryRequired
		if !cancel && !waiting {
			return nil
		}
		if op.SourceRetiredAt != nil || op.SourceStopIntentAt != nil || op.SourceStop != nil {
			return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_RECOVERY_REQUIRED", "Shutdown intent exists; retain this operation and verify the same native controller before recovery", nil)
		}
		if !cancel && op.Phase == domain.ReplacementRecoveryRequired {
			return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_RECOVERY_REQUIRED", "No safe retained phase is available; verify the original source before recovery", nil)
		}
		if !cancel {
			if err := s.checkProject(locked, op); err != nil {
				return err
			}
		}
		if !cancel {
			source, found, err := s.deps.Store.GetSession(locked, op.SourceSessionID)
			if err != nil {
				return err
			}
			if !found || source.ProjectID != op.ProjectID || source.IsTerminated || source.ControllerOwner() != op.SourceOwner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
				return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_SOURCE_CHANGED", "Source owner changed before reset", nil)
			}
			observed, err := s.deps.Runtime.ObserveReplacementSource(locked, op)
			if err != nil {
				return err
			}
			if observed.Owner != op.SourceOwner || observed.NativeID != sourceNativeID(op.SourceOwner) || observed.Provenance != domain.NativeThreadMain || observed.ObservedAt.IsZero() || (observed.State != ports.ChatControllerReady && observed.State != ports.ChatControllerBusy) {
				return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_SOURCE_UNVERIFIED", "Source controller cannot be positively confirmed", nil)
			}
			if err := positiveIdle(op, observed); err != nil {
				return err
			}
		}
		now := s.deps.Now().UTC()
		priorAttempts := len(op.Attempts)
		_, applied, actionErr := s.deps.Store.ResetReplacementBeforeStop(locked, op.ID, op.Revision, cancel, now, now.Add(operationDeadline))
		current, err := s.Get(locked, id)
		if err != nil {
			return err
		}
		op = current
		expectedPhase := domain.ReplacementPreflight
		if cancel {
			expectedPhase = domain.ReplacementCancelled
		}
		if op.Phase == expectedPhase && len(op.Attempts) == priorAttempts+1 && op.Attempts[priorAttempts].EndedAt.Equal(now) {
			return nil
		}
		if errors.Is(actionErr, domain.ErrReplacementSourceBusy) {
			return apierr.Conflict("ORCHESTRATOR_HANDOFF_CONTROL_UNSETTLED", "The previous preparation has a provider reservation; verify its receipt before retry", nil)
		}
		if actionErr != nil {
			return actionErr
		}
		if !applied {
			return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_SOURCE_CHANGED", "Source, lease or operation changed before reset", nil)
		}
		return nil
	}
	if s.deps.WithProjectGate != nil {
		err = s.deps.WithProjectGate(ctx, op.ProjectID, run)
	} else {
		unlock := s.lockProject(op.ProjectID)
		defer unlock()
		err = run(ctx)
	}
	return op, err
}
