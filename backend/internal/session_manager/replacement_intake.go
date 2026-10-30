package sessionmanager

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// DrainReplacementIntake requests a canonical queue kick under the current
// writer fence. It never starts, resumes, stops or probes a native conversation.
func (m *Manager) DrainReplacementIntake(ctx context.Context, op domain.OrchestratorReplacement) error {
	ctx, release, err := m.acquireOrchestratorGate(ctx, op.ProjectID)
	if err != nil {
		return err
	}
	defer release()
	if op.Phase != domain.ReplacementNotifying || op.WriterActivation == nil || op.IntakeReleasedAt == nil {
		return domain.ErrReplacementStateInvalid
	}
	observed, err := m.ObserveReplacementSuccessor(ctx, op)
	if err != nil {
		return err
	}
	proof, verified := op.ExpectedSuccessorController()
	if rec, found, err := m.store.GetSession(ctx, op.SuccessorSessionID); err != nil {
		return err
	} else if found && op.ActivatedSuccessorMatches(rec) {
		proof.Owner, verified = rec.ControllerOwner(), true
	}
	if !verified || observed.Owner != proof.Owner || observed.NativeID != proof.Owner.ProviderConversationID || observed.Provenance != domain.NativeThreadMain || observed.ObservedAt.IsZero() || (observed.State != ports.ChatControllerReady && observed.State != ports.ChatControllerBusy) {
		return domain.ErrReplacementStateInvalid
	}
	reader, ok := m.store.(interface {
		ReplacementWakeFenced(context.Context, domain.SessionID) (bool, error)
	})
	if !ok {
		return domain.ErrReplacementStateInvalid
	}
	fenced, err := reader.ReplacementWakeFenced(ctx, op.SuccessorSessionID)
	if err != nil {
		return err
	}
	if fenced {
		return domain.ErrReplacementIntakeFenced
	}
	return m.chat.DrainChatQueue(ctx, op.SuccessorSessionID)
}
