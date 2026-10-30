package chat

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// EnsureReplacementChatBootstrap sends only the exact persisted bootstrap intent.
// Reconciliation can create a turn only when its canonical client ID has no
// message at all. An accepted-but-unverifiable turn still fails closed.
func (s *Service) EnsureReplacementChatBootstrap(ctx context.Context, expected domain.OrchestratorReplacement, prompt string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	if expected.Phase != domain.ReplacementControllerReady {
		return ports.ReplacementPreparationDelivery{}, domain.ErrReplacementStateInvalid
	}
	return s.ensureReplacementSuccessorControl(ctx, expected, prompt, allowNew)
}

// EnsureReplacementChatProbe uses the resumed owner, never the first launch.
func (s *Service) EnsureReplacementChatProbe(ctx context.Context, expected domain.OrchestratorReplacement, prompt string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	if expected.Phase != domain.ReplacementVerifyingResume {
		return ports.ReplacementPreparationDelivery{}, domain.ErrReplacementStateInvalid
	}
	return s.ensureReplacementSuccessorControl(ctx, expected, prompt, allowNew)
}

func (s *Service) ensureReplacementSuccessorControl(ctx context.Context, expected domain.OrchestratorReplacement, prompt string, _ bool) (ports.ReplacementPreparationDelivery, error) {
	var receipt ports.ReplacementPreparationDelivery
	op, rec, controller, err := s.replacementChatSuccessor(ctx, expected)
	if err != nil {
		return receipt, err
	}
	if op.Phase == domain.ReplacementVerifyingResume && (op.ResumeEvidence == nil || op.ResumeEvidence.ResumedAt == nil || op.ResumeEvidence.Probe == nil) {
		return receipt, domain.ErrReplacementStateInvalid
	}
	clientID, promptHash, turnID := replacementSuccessorControl(op)
	if controller == nil || (op.Phase != domain.ReplacementControllerReady && op.Phase != domain.ReplacementVerifyingResume) || clientID == "" || promptHash == "" || promptHash != replacementPromptHash(prompt) {
		return receipt, domain.ErrReplacementStateInvalid
	}
	receipt.Owner, receipt.NativeID = rec.ControllerOwner(), controller.ProviderConversationID()
	turn, err := s.replacementControlTurn(ctx, controller, rec.ID, clientID, promptHash, turnID)
	if err != nil {
		return receipt, err
	}
	if turn != nil {
		receipt.TurnID = turn.ID
		return receipt, nil
	}
	if turnID != "" {
		receipt.Uncertain = true
		return receipt, nil
	}
	// replacementControlTurn returned (nil, nil) only because the client ID
	// has no canonical message. Send commits that unique row before dispatch,
	// so retrying this persisted intent cannot duplicate a provider delivery.
	observation, err := s.ObserveReplacementChatSuccessor(ctx, op)
	if err != nil {
		return receipt, err
	}
	if observation.State != ports.ChatControllerReady || observation.PendingApproval || observation.ActiveTurn != nil {
		return receipt, domain.ErrReplacementSourceBusy
	}
	ctx = context.WithValue(ctx, replacementControlContextKey{}, replacementControlGrant{session: rec.ID, generation: controller.Generation(), clientID: clientID, promptHash: promptHash})
	if _, err := controller.Send(ctx, ports.ChatUserMessage{Text: prompt, ClientMessageID: clientID, Origin: domain.MessageOriginAutomation}); err != nil {
		return receipt, err
	}
	if _, _, _, err := s.replacementChatSuccessor(ctx, op); err != nil {
		return receipt, err
	}
	turn, err = s.replacementControlTurn(ctx, controller, rec.ID, clientID, promptHash, turnID)
	if err == nil && turn != nil {
		receipt.TurnID = turn.ID
	} else {
		receipt.Uncertain = true
	}
	return receipt, err
}

func (s *Service) replacementSuccessorControlTurn(ctx context.Context, op domain.OrchestratorReplacement, controller *Controller) (*domain.ConversationTurn, error) {
	clientID, promptHash, turnID := replacementSuccessorControl(op)
	if clientID == "" {
		return nil, nil
	}
	return s.replacementControlTurn(ctx, controller, op.SuccessorSessionID, clientID, promptHash, turnID)
}

func replacementSuccessorControl(op domain.OrchestratorReplacement) (string, string, string) {
	if op.ResumeEvidence != nil && op.ResumeEvidence.ResumedAt != nil {
		p := op.ResumeEvidence.Probe
		if p == nil || p.IntentAt.IsZero() {
			return "", "", ""
		}
		return op.ResumeProbeClientID, p.PromptHash, p.TurnID
	}
	if op.Bootstrap == nil || op.Bootstrap.IntentAt.IsZero() {
		return "", "", ""
	}
	return op.BootstrapClientID, op.Bootstrap.PromptHash, op.Bootstrap.TurnID
}
