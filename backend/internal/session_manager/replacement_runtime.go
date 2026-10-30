package sessionmanager

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementChatRuntime interface {
	ObserveReplacementChatSource(context.Context, domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error)
	EnsureReplacementChatPreparation(context.Context, domain.OrchestratorReplacement, string, bool) (ports.ReplacementPreparationDelivery, error)
	QuiesceReplacementChatSource(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error)
}

func (m *Manager) beginReplacementSourceAction(ctx context.Context, expected domain.OrchestratorReplacement) (context.Context, domain.SessionRecord, func(), error) {
	locked, release, err := m.acquireOrchestratorGate(ctx, expected.ProjectID)
	if err != nil {
		return ctx, domain.SessionRecord{}, func() {}, err
	}
	fail := func(err error) (context.Context, domain.SessionRecord, func(), error) {
		release()
		return ctx, domain.SessionRecord{}, func() {}, err
	}
	reader, ok := m.store.(replacementAdmissionReader)
	if !ok {
		return fail(domain.ErrReplacementStateInvalid)
	}
	op, found, err := reader.GetActiveOrchestratorReplacement(locked, expected.ProjectID)
	if err != nil {
		return fail(err)
	}
	if !found || op.ID != expected.ID || op.Revision != expected.Revision || op.Phase != expected.Phase || op.SourceRetiredAt != nil || op.SourceSessionID != expected.SourceSessionID || op.SourceOwner != expected.SourceOwner || op.SourceCapabilityVerifier != expected.SourceCapabilityVerifier {
		return fail(domain.ErrReplacementSourceChanged)
	}
	rec, found, err := m.store.GetSession(locked, op.SourceSessionID)
	if err != nil {
		return fail(err)
	}
	if !found || rec.IsTerminated || rec.Kind != domain.KindOrchestrator || rec.ProjectID != op.ProjectID || rec.ControllerOwner() != op.SourceOwner || op.SourceCapabilityVerifier == "" || rec.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return fail(domain.ErrReplacementSourceChanged)
	}
	if rec.Harness != domain.HarnessClaudeCode {
		return fail(&ports.NativeTranscriptError{Code: "ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED", Reason: "Phase 1 requires a Claude Code source orchestrator"})
	}
	return locked, rec, release, nil
}

// ObserveReplacementSource shares admission with lifecycle commands and only
// delegates observations for the operation's current bound source controller.
func (m *Manager) ObserveReplacementSource(ctx context.Context, op domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	ctx, rec, release, err := m.beginReplacementSourceAction(ctx, op)
	if err != nil {
		return ports.ReplacementSourceObservation{}, err
	}
	defer release()
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		chat, ok := m.chat.(replacementChatRuntime)
		if !ok {
			return ports.ReplacementSourceObservation{}, ports.ErrChatUnsupported
		}
		return chat.ObserveReplacementChatSource(ctx, op)
	}
	return m.observeReplacementTUISource(ctx, op, rec)
}

// EnsureReplacementPreparation preserves lookup-only recovery under the same
// project reservation; neither an uncertain receipt nor a stale epoch resends.
func (m *Manager) EnsureReplacementPreparation(ctx context.Context, op domain.OrchestratorReplacement, prompt string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	ctx, rec, release, err := m.beginReplacementSourceAction(ctx, op)
	if err != nil {
		return ports.ReplacementPreparationDelivery{}, err
	}
	defer release()
	if op.Phase != domain.ReplacementPreparingHandoff {
		return ports.ReplacementPreparationDelivery{}, domain.ErrReplacementStateInvalid
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		chat, ok := m.chat.(replacementChatRuntime)
		if !ok {
			return ports.ReplacementPreparationDelivery{}, ports.ErrChatUnsupported
		}
		return chat.EnsureReplacementChatPreparation(ctx, op, prompt, allowNew)
	}
	return m.ensureReplacementTUIPreparation(ctx, op, rec, prompt, allowNew)
}

// QuiesceReplacementSource parks only a source in the frozen export stage.
// The Chat adapter supplies a confirmed host shutdown receipt, never a marker.
func (m *Manager) QuiesceReplacementSource(ctx context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
	ctx, rec, release, err := m.beginReplacementSourceAction(ctx, op)
	if err != nil {
		return domain.ReplacementControllerStop{}, err
	}
	defer release()
	if op.Phase != domain.ReplacementExportingTranscript {
		return domain.ReplacementControllerStop{}, domain.ErrReplacementStateInvalid
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		chat, ok := m.chat.(replacementChatRuntime)
		if !ok {
			return domain.ReplacementControllerStop{}, ports.ErrChatUnsupported
		}
		return chat.QuiesceReplacementChatSource(ctx, op)
	}
	return m.quiesceReplacementTUISource(ctx, op, rec)
}
