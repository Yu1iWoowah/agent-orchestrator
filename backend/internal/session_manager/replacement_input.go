package sessionmanager

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

// AcquireTerminalInput admits raw human keystrokes separately from the internal
// input lease used by operation-owned preparation. Keep the project reservation
// through the actual PTY write, so acceptance cannot race a buffered keystroke.
func (m *Manager) AcquireTerminalInput(ctx context.Context, id domain.SessionID) (func(), error) {
	_, releaseProject, err := m.beginReplacementInput(ctx, id)
	if errors.Is(err, domain.ErrReplacementInProgress) {
		return nil, fmt.Errorf("ORCHESTRATOR_REPLACEMENT_INTAKE_FENCED: %w", domain.ErrReplacementIntakeFenced)
	}
	if err != nil {
		return nil, err
	}
	releaseInput, ok := m.AcquireSessionInput(id)
	if !ok {
		releaseProject()
		return nil, errors.New("session input is disabled while an exclusive session operation is in progress")
	}
	return func() { releaseInput(); releaseProject() }, nil
}

// BeginReplacementMutation admits a public Chat mutation before controller
// locks. Operation-owned control turns do not enter this public boundary.
func (m *Manager) BeginReplacementMutation(ctx context.Context, id domain.SessionID) (context.Context, func(), error) {
	locked, release, err := m.beginReplacementInput(ctx, id)
	if errors.Is(err, domain.ErrReplacementInProgress) {
		err = domain.ErrReplacementIntakeFenced
	}
	return locked, release, err
}

// Only input can use a durable release. Generic clean/restore/account actions
// keep the stricter project reservation throughout a paused handoff.
func (m *Manager) beginReplacementInput(ctx context.Context, id domain.SessionID) (context.Context, func(), error) {
	locked, release, err := m.beginReplacementSessionAction(ctx, id)
	if !errors.Is(err, domain.ErrReplacementInProgress) {
		return locked, release, err
	}
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil {
		return ctx, func() {}, err
	}
	if !found {
		return ctx, func() {}, domain.ErrReplacementSourceChanged
	}
	locked, release, err = m.acquireOrchestratorGate(ctx, rec.ProjectID)
	if err != nil {
		return ctx, func() {}, err
	}
	fail := func(err error) (context.Context, func(), error) {
		release()
		return ctx, func() {}, err
	}
	reader, ok := m.store.(interface {
		GetActiveOrchestratorReplacement(context.Context, domain.ProjectID) (domain.OrchestratorReplacement, bool, error)
		ReplacementWakeFenced(context.Context, domain.SessionID) (bool, error)
	})
	if !ok {
		return fail(domain.ErrReplacementInProgress)
	}
	op, active, err := reader.GetActiveOrchestratorReplacement(locked, rec.ProjectID)
	if err != nil {
		return fail(err)
	}
	current, found, err := m.store.GetSession(locked, id)
	if err != nil {
		return fail(err)
	}
	allowed := active && found && !current.IsTerminated && current.ProjectID == op.ProjectID && current.Kind == domain.KindOrchestrator
	source := op.CanReleaseSourceIntake() && op.SourceIntakeReleasedAt != nil && current.ID == op.SourceSessionID && current.ControllerOwner() == op.SourceOwner && current.Metadata.BrowserCapabilityVerifier == op.SourceCapabilityVerifier
	proof, verified := op.ExpectedSuccessorController()
	target := op.Phase == domain.ReplacementNotifying && op.IntakeReleasedAt != nil && op.WriterActivation != nil && verified && current.ID == op.SuccessorSessionID && current.ControllerOwner() == proof.Owner && current.Metadata.BrowserCapabilityVerifier == proof.CapabilityVerifier
	if !allowed || !source && !target {
		return fail(domain.ErrReplacementInProgress)
	}
	fenced, err := reader.ReplacementWakeFenced(locked, id)
	if err != nil {
		return fail(err)
	}
	if fenced {
		return fail(domain.ErrReplacementInProgress)
	}
	return locked, release, nil
}

func (m *Manager) queueReplacementTUIInput(ctx context.Context, id domain.SessionID, message, clientID string, options ports.MessageDeliveryOptions) (context.Context, func(), bool, error) {
	noop := func() {}
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil || !found || rec.Kind != domain.KindOrchestrator || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeTUI {
		return ctx, noop, false, err
	}
	locked, release, err := m.acquireOrchestratorGate(ctx, rec.ProjectID)
	if err != nil {
		return ctx, noop, false, err
	}
	reader, ok := m.store.(replacementAdmissionReader)
	if !ok {
		return locked, release, false, nil
	}
	op, active, err := reader.GetActiveOrchestratorReplacement(locked, rec.ProjectID)
	if err != nil || !active {
		return locked, release, false, err
	}
	rec, found, err = m.store.GetSession(locked, id)
	if err != nil {
		return locked, release, false, err
	}
	expectedOwner := op.SourceOwner
	expectedOwner.IsTerminated = op.SourceRetiredAt != nil
	if !found || rec.ID != op.SourceSessionID || rec.ProjectID != op.ProjectID || rec.ControllerOwner() != expectedOwner || rec.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return locked, release, false, domain.ErrReplacementSourceChanged
	}
	if op.CanReleaseSourceIntake() && op.SourceIntakeReleasedAt != nil {
		if fence, ok := m.store.(interface {
			ReplacementWakeFenced(context.Context, domain.SessionID) (bool, error)
		}); ok {
			fenced, err := fence.ReplacementWakeFenced(locked, id)
			if err != nil || !fenced {
				return locked, release, false, err
			}
		}
	}
	if proof, _ := ctx.Value(replacementTUIWriteProofKey{}).(func(context.Context, domain.SessionRecord) error); proof != nil && op.Phase == domain.ReplacementPreparingHandoff && op.Preparation != nil && clientID == op.Preparation.ControlClientID {
		return locked, release, false, nil
	}
	// A semantic report already has a durable report-batch receipt. Refuse it
	// as busy so that coordinator retains/retries the same identity; a queue
	// append must not masquerade as native semantic acceptance.
	if clientID != "" {
		return locked, release, false, domain.ErrReplacementIntakeFenced
	}
	queue, ok := m.chat.(interface {
		QueueReplacementIntake(context.Context, domain.SessionID, ports.ChatUserMessage) (domain.ConversationTurn, error)
	})
	if !ok {
		return locked, release, false, domain.ErrReplacementIntakeFenced
	}
	origin := domain.MessageOriginAutomation
	if options.AuthoredByUser {
		origin = domain.MessageOriginHuman
	}
	_, err = queue.QueueReplacementIntake(locked, id, ports.ChatUserMessage{Text: message, ClientMessageID: "session-send:" + m.newLaunchID(), Origin: origin, AuthoredByUser: options.AuthoredByUser, SenderSessionID: options.SenderSessionID, InteractionAt: options.InteractionAt})
	return locked, release, err == nil, err
}

type replacementTUIWriteProofKey struct{}

// Both the initial paste and confirmActive's Enter retries use this same
// private proof. Public sends retain their existing guarded delivery path.
func (m *Manager) deliverTUIPrompt(ctx context.Context, guard *sessionguard.Guard, id domain.SessionID, text string, composer func(context.Context, domain.SessionRecord) (bool, func()), after func(context.Context) error) (sessionguard.Outcome, error) {
	proof, _ := ctx.Value(replacementTUIWriteProofKey{}).(func(context.Context, domain.SessionRecord) error)
	if proof == nil {
		return guard.DeliverWithComposerCheck(ctx, id, text, composer, after)
	}
	return guard.DeliverWithOwnershipCheck(ctx, id, text, proof, composer, after)
}
