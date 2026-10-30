package orchestratorreplacement

import (
	"context"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (s *Service) beginResumeVerification(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	if err := s.checkProject(ctx, op); err != nil {
		return op, err
	}
	if !s.deps.Now().Before(op.DeadlineAt) {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_RESUME_TIMEOUT", "Native resume verification exceeded the operation deadline", nil)
	}
	p, b := op.SuccessorController, op.Bootstrap
	if op.SourceRetiredAt == nil || p == nil || b == nil || b.SettledAt == nil || b.TurnID == "" || op.ResumeEvidence != nil {
		return op, domain.ErrReplacementStateInvalid
	}
	observed, err := s.deps.Runtime.ObserveReplacementSuccessor(ctx, op)
	if err != nil {
		return op, err
	}
	if observed.Owner != p.Owner || observed.NativeID != p.Owner.ProviderConversationID || observed.Provenance != domain.NativeThreadMain || observed.ObservedAt.IsZero() || observed.ControlTurn == nil || observed.ControlTurn.ID != b.TurnID || observed.ControlTurn.HandledBySessionID != op.SuccessorSessionID || observed.ControlTurn.State != domain.TurnStateCompleted {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_BOOTSTRAP_UNVERIFIED", "Settled bootstrap no longer matches the successor native main line", nil)
	}
	if observed.State != ports.ChatControllerReady || observed.PendingApproval || observed.ActiveTurn != nil && !observed.ActiveTurn.State.Terminal() {
		return op, nil
	}
	op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: s.deps.Now().UTC(), BeforeOwner: p.Owner, BeforeNativeID: observed.NativeID, BeforeCapabilityVerifier: p.CapabilityVerifier}
	op.Phase = domain.ReplacementVerifyingResume
	return s.save(ctx, op)
}

func (s *Service) verifyResume(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	if err := s.checkProject(ctx, op); err != nil {
		return op, err
	}
	if !s.deps.Now().Before(op.DeadlineAt) {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_RESUME_TIMEOUT", "Native resume verification exceeded the operation deadline", nil)
	}
	p := op.ResumeEvidence
	if op.SourceRetiredAt == nil || p == nil || p.StartedAt.IsZero() {
		return op, domain.ErrReplacementStateInvalid
	}
	if p.StopConfirmed {
		return s.resumeNativeSuccessor(ctx, op)
	}
	stop, err := s.deps.Runtime.QuiesceReplacementSuccessor(ctx, op)
	if err != nil {
		return op, err
	}
	rec, found, err := s.deps.Store.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		return op, err
	}
	if !found || rec.ProjectID != op.ProjectID || stop.Owner != p.BeforeOwner || stop.NativeID != p.BeforeNativeID || stop.CapabilityVerifier != p.BeforeCapabilityVerifier || !stop.MatchesSource(rec) {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_SUCCESSOR_STOP_UNVERIFIED", "Successor shutdown receipt differs from its current controller", nil)
	}
	p.StopConfirmed, p.StopReceipt = true, &stop
	return s.save(ctx, op)
}

func (s *Service) resumeNativeSuccessor(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	p := op.ResumeEvidence
	if p.StopReceipt == nil {
		return op, domain.ErrReplacementStateInvalid
	}
	if p.ResumedAt != nil {
		return s.probeSuccessor(ctx, op)
	}
	allowNew := false
	if p.ResumeIntentAt == nil && p.ResumeGeneration == "" {
		now := s.deps.Now().UTC()
		p.ResumeIntentAt, p.ResumeGeneration = &now, uuid.NewString()
		var err error
		op, err = s.save(ctx, op)
		if err != nil {
			return op, err
		}
		allowNew = true
	} else if p.ResumeIntentAt == nil || p.ResumeGeneration == "" {
		return op, domain.ErrReplacementStateInvalid
	}
	observed, err := s.deps.Runtime.ResumeReplacementSuccessor(ctx, op, allowNew)
	if err != nil {
		return op, err
	}
	rec, found, err := s.deps.Store.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		return op, err
	}
	if !found || rec.ProjectID != op.ProjectID || rec.Kind != domain.KindOrchestrator || rec.IsTerminated || rec.HibernatedAt != nil || rec.Harness != domain.HarnessClaudeCode || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat || rec.ControllerOwner() != observed.Owner || observed.Owner.ControllerGeneration != p.ResumeGeneration || observed.Owner.ControllerGeneration == p.BeforeOwner.ControllerGeneration || observed.NativeID == "" || observed.NativeID != p.BeforeNativeID || observed.NativeID != rec.Metadata.ProviderConversationID || observed.CapabilityVerifier == "" || observed.CapabilityVerifier == p.BeforeCapabilityVerifier || observed.CapabilityVerifier != rec.Metadata.BrowserCapabilityVerifier || !observed.HistoryVerified || observed.ObservedAt.IsZero() {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_NATIVE_RESUME_UNVERIFIED", "Required replay did not return the reserved successor owner on the same native thread", nil)
	}
	p.AfterOwner, p.AfterNativeID, p.AfterCapabilityVerifier = observed.Owner, observed.NativeID, observed.CapabilityVerifier
	p.HistoryVerified, p.ResumedAt = true, &observed.ObservedAt
	return s.save(ctx, op)
}
