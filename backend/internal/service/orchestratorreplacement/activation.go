package orchestratorreplacement

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (s *Service) activateWriter(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	if err := s.checkProject(ctx, op); err != nil {
		return op, err
	}
	if !s.deps.Now().Before(op.DeadlineAt) {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_ACTIVATION_TIMEOUT", "Writer activation exceeded the operation deadline", nil)
	}
	proof, ok := op.ExpectedSuccessorController()
	p := op.ResumeEvidence
	if !ok || p == nil || p.VerifiedAt == nil || p.Probe == nil || p.Probe.SettledAt == nil {
		return op, domain.ErrReplacementStateInvalid
	}
	observed, err := s.deps.Runtime.ObserveReplacementSuccessor(ctx, op)
	if err != nil {
		return op, err
	}
	if observed.Owner != proof.Owner || observed.NativeID != p.AfterNativeID || observed.Provenance != domain.NativeThreadMain || observed.ObservedAt.IsZero() || observed.ControlTurn == nil || observed.ControlTurn.ID != p.Probe.TurnID || observed.ControlTurn.HandledBySessionID != op.SuccessorSessionID || observed.ControlTurn.State != domain.TurnStateCompleted || !observed.ControlAcknowledged {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_ACTIVATION_UNVERIFIED", "Current successor no longer verifies the main continuity probe", nil)
	}
	if observed.State != ports.ChatControllerReady || observed.PendingApproval || observed.ActiveTurn != nil && !observed.ActiveTurn.State.Terminal() {
		return op, nil
	}
	reserved, cancel := context.WithDeadline(ctx, op.DeadlineAt)
	defer cancel()
	_, actionErr := s.deps.Documents.ActivateReplacementWriter(reserved, op.ID, op.Revision)
	current, err := s.Get(ctx, op.ID)
	if err != nil {
		return op, err
	}
	if current.WriterActivation != nil && current.Phase == domain.ReplacementNotifying {
		return current, nil
	}
	if actionErr != nil {
		return current, actionErr
	}
	return current, apierr.Conflict("ORCHESTRATOR_HANDOFF_ACTIVATION_UNVERIFIED", "Writer activation did not publish a durable receipt", nil)
}
