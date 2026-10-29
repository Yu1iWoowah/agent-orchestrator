package orchestratorreplacement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

func (s *Service) prepare(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	p := op.Preparation
	if p == nil {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_NOT_PREPARING", "Preparation intent is missing", nil)
	}
	if !s.deps.Now().Before(p.DeadlineAt) {
		return s.wait(ctx, op, domain.ReplacementAwaitingHandoff, op.Phase, apierr.Conflict("ORCHESTRATOR_HANDOFF_PREPARATION_TIMEOUT", "Preparation did not settle before its deadline", nil))
	}
	observation, err := s.deps.Runtime.ObserveReplacementSource(ctx, op)
	if err != nil {
		if pendingSourceObservation(err) {
			return s.wait(ctx, op, op.Phase, op.Phase, err)
		}
		return op, err
	}
	if observation.Owner != op.SourceOwner || observation.Provenance != domain.NativeThreadMain || observation.NativeID == "" || observation.NativeID != sourceNativeID(op.SourceOwner) || observation.ObservedAt.IsZero() {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_SOURCE_UNVERIFIED", "Preparation controller identity is not verified", nil)
	}
	if turn := observation.ControlTurn; turn != nil {
		if turn.ID == "" || turn.HandledBySessionID != op.SourceSessionID || p.ControlTurnID != "" && p.ControlTurnID != turn.ID {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_TURN_MISMATCH", "Preparation receipt refers to another control turn", nil)
		}
		if p.ControlTurnID == "" {
			p.ControlTurnID = turn.ID
			op, err = s.save(ctx, op)
			if err != nil {
				return op, err
			}
			p = op.Preparation
		}
		if !turn.State.Terminal() || observation.PendingApproval {
			return s.wait(ctx, op, domain.ReplacementPreparingHandoff, op.Phase, apierr.Conflict("ORCHESTRATOR_HANDOFF_TURN_PENDING", "Preparation turn or approval is still pending", nil))
		}
		if turn.State != domain.TurnStateCompleted {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_TURN_FAILED", "Preparation turn did not complete successfully", nil)
		}
		if err := positiveIdle(op, observation); err != nil {
			return s.wait(ctx, op, domain.ReplacementPreparingHandoff, op.Phase, err)
		}
		if p.SubmittedAt == nil || p.WriteReceiptID == "" || p.SubmittedHash == "" {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_RECEIPT_MISSING", "Successful preparation still requires its managed write receipt", nil)
		}
		sessions, err := s.deps.Store.ListSessions(ctx, op.ProjectID)
		if err != nil {
			return op, err
		}
		for _, session := range sessions {
			if session.Kind == domain.KindWorker && !session.IsTerminated && !slices.Contains(p.ExpectedSessions, session.ID) {
				return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_SESSION_MAP_CHANGED", "A new worker requires an updated section B before freeze", nil)
			}
		}
		if p.IntakeRevision != op.IntakeRevision {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_INTAKE_CHANGED", "New instructions require a fresh handoff preparation", nil)
		}
		now := s.deps.Now().UTC()
		p.VerifiedAt = &now
		op.Phase, op.Error, op.ResumePhase = domain.ReplacementHandoffVerified, nil, ""
		return s.save(ctx, op)
	}
	if p.ControlTurnID != "" {
		return s.wait(ctx, op, domain.ReplacementPreparingHandoff, op.Phase, apierr.Conflict("ORCHESTRATOR_HANDOFF_TURN_PENDING", "Accepted preparation has no verified settlement yet", nil))
	}
	if err := positiveIdle(op, observation); err != nil {
		return s.wait(ctx, op, domain.ReplacementPreparingHandoff, op.Phase, err)
	}
	allowNew := false
	if p.PromptIntentAt == nil {
		now := s.deps.Now().UTC()
		p.PromptIntentAt = &now
		hash := sha256.Sum256([]byte(preparationPrompt(op)))
		p.PromptHash = hex.EncodeToString(hash[:])
		p.IntakeRevision = op.IntakeRevision
		op, err = s.save(ctx, op)
		if err != nil {
			return op, err
		}
		allowNew = true
	}
	delivery, err := s.deps.Runtime.EnsureReplacementPreparation(ctx, op, preparationPrompt(op), allowNew)
	if err != nil {
		return op, err
	}
	if delivery.Uncertain || delivery.TurnID == "" || delivery.Owner != op.SourceOwner || delivery.NativeID != sourceNativeID(op.SourceOwner) {
		return s.wait(ctx, op, domain.ReplacementAwaitingSource, op.Phase, apierr.Conflict("ORCHESTRATOR_HANDOFF_PREPARATION_UNCERTAIN", "Preparation delivery cannot be confirmed; do not blindly resend", nil))
	}
	op.Preparation.ControlTurnID = delivery.TurnID
	op.Error, op.ResumePhase = nil, ""
	return s.save(ctx, op)
}
