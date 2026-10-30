package sessionmanager

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ObserveReplacementSuccessor reads the already-activated intake owner. Earlier
// successor lifecycle stages and the concrete native observer are added in 3C.
func (m *Manager) ObserveReplacementSuccessor(ctx context.Context, expected domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	var zero ports.ReplacementSourceObservation
	ctx, release, err := m.acquireOrchestratorGate(ctx, expected.ProjectID)
	if err != nil {
		return zero, err
	}
	defer release()
	reader, ok := m.store.(replacementAdmissionReader)
	if !ok {
		return zero, domain.ErrReplacementStateInvalid
	}
	op, found, err := reader.GetActiveOrchestratorReplacement(ctx, expected.ProjectID)
	if err != nil {
		return zero, err
	}
	if !found || op.ID != expected.ID || op.Revision != expected.Revision || op.Phase != expected.Phase || op.Phase != domain.ReplacementNotifying || op.SourceSessionID != expected.SourceSessionID || op.SourceRetiredAt == nil || op.WriterActivation == nil || op.SuccessorSessionID == "" || op.SuccessorSessionID != expected.SuccessorSessionID || op.SpawnRequestID != expected.SpawnRequestID || op.SpawnRequestHash != expected.SpawnRequestHash {
		return zero, domain.ErrReplacementStateInvalid
	}
	proof, verified := op.ExpectedSuccessorController()
	if !verified {
		return zero, domain.ErrReplacementStateInvalid
	}
	rec, found, err := m.store.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		return zero, err
	}
	if !found || rec.IsTerminated || rec.ProjectID != op.ProjectID || rec.Kind != domain.KindOrchestrator || rec.Harness != domain.HarnessClaudeCode || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat || rec.ProvisionState != domain.SessionProvisionReady || rec.ClientRequestID != op.SpawnRequestID || rec.ClientRequestHash != op.SpawnRequestHash || rec.ControllerOwner() != proof.Owner || rec.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
		return zero, domain.ErrReplacementStateInvalid
	}
	observer, ok := m.chat.(interface {
		ObserveReplacementChatSuccessor(context.Context, domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error)
	})
	if !ok {
		return zero, ports.ErrChatUnsupported
	}
	return observer.ObserveReplacementChatSuccessor(ctx, op)
}

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
