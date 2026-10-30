package orchestratorreplacement

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (s *Service) releaseIntake(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	if op.WriterActivation == nil {
		return op, domain.ErrReplacementStateInvalid
	}
	if op.IntakeDrainRequestedAt != nil {
		proof, ok := op.ExpectedSuccessorController()
		target, found, err := s.deps.Store.GetSession(ctx, op.SuccessorSessionID)
		if err != nil {
			return op, err
		}
		if !ok || !found || target.ProjectID != op.ProjectID || target.Kind != domain.KindOrchestrator || target.ControllerOwner() != proof.Owner || target.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_SUCCESSOR_OWNER_CHANGED", "Activated successor ownership changed after the queue kick", nil)
		}
		fenced, err := s.deps.Store.ReplacementWakeFenced(ctx, target.ID)
		if err != nil {
			return op, err
		}
		if fenced {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_SUCCESSOR_OWNER_CHANGED", "Activated successor writer lease changed after the queue kick", nil)
		}
		if len(op.Notifications) == 0 {
			op.Phase = domain.ReplacementCompleted
			return s.save(ctx, op)
		}
		return op, nil
	}
	if op.IntakeReleasedAt == nil {
		if err := s.checkProject(ctx, op); err != nil {
			return op, err
		}
		observed, err := s.deps.Runtime.ObserveReplacementSuccessor(ctx, op)
		if err != nil {
			return op, err
		}
		proof, ok := op.ExpectedSuccessorController()
		if !ok || observed.Owner != proof.Owner || observed.NativeID != proof.Owner.ProviderConversationID || observed.Provenance != domain.NativeThreadMain || observed.ObservedAt.IsZero() {
			return op, domain.ErrReplacementStateInvalid
		}
		if observed.State != ports.ChatControllerReady || observed.PendingApproval || observed.ActiveTurn != nil && !observed.ActiveTurn.State.Terminal() {
			return op, nil
		}
		_, _, actionErr := s.deps.Store.ReleaseReplacementIntake(ctx, op.ID, op.Revision, s.deps.Now().UTC())
		current, err := s.Get(ctx, op.ID)
		if err != nil {
			return op, err
		}
		op = current
		if op.IntakeReleasedAt == nil {
			if actionErr != nil {
				return op, actionErr
			}
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_INTAKE_RELEASE_UNVERIFIED", "Resumed writer changed before intake release", nil)
		}
	}
	// Repeatable kicks use canonical queue claims. No turn or payload is copied,
	// and a lost drain reply never authorizes resending a provider turn.
	if err := s.deps.Runtime.DrainReplacementIntake(ctx, op); err != nil {
		return s.wait(ctx, op, domain.ReplacementNotifying, domain.ReplacementNotifying, apierr.Conflict("ORCHESTRATOR_HANDOFF_INTAKE_DRAIN_PENDING", "Released intake is pending a verified queue drain", nil))
	}
	_, _, actionErr := s.deps.Store.RecordReplacementIntakeDrain(ctx, op.ID, op.Revision, s.deps.Now().UTC())
	current, err := s.Get(ctx, op.ID)
	if err != nil {
		return op, err
	}
	if current.IntakeDrainRequestedAt != nil {
		return current, nil
	}
	if actionErr != nil {
		return current, actionErr
	}
	return current, apierr.Conflict("ORCHESTRATOR_HANDOFF_INTAKE_DRAIN_UNVERIFIED", "Writer changed before the queue-kick receipt could be recorded", nil)
}
