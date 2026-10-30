package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.ReplacementChatRuntime = (*Service)(nil)

type replacementControlContextKey struct{}
type replacementControlGrant struct {
	session    domain.SessionID
	generation string
	clientID   string
	promptHash string
}

func replacementPromptHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// A renderer or relay may supply client IDs but cannot supply this private
// context grant. Enforce it at both durable queue entry points.
func validateReplacementControlMessage(ctx context.Context, id domain.SessionID, generation string, msg ports.ChatUserMessage) error {
	if !strings.HasPrefix(msg.ClientMessageID, "replacement:") && !strings.HasPrefix(msg.ClientMessageID, "replacement/") {
		return nil
	}
	grant, ok := ctx.Value(replacementControlContextKey{}).(replacementControlGrant)
	if !ok || grant.session != id || grant.generation != generation || grant.clientID != msg.ClientMessageID || grant.promptHash != replacementPromptHash(msg.Text) || msg.Origin != domain.MessageOriginAutomation || msg.AuthoredByUser || msg.SenderSessionID != "" || len(msg.Content) != 0 || len(msg.Excerpts) != 0 || msg.Continuation {
		return domain.ErrReplacementIntakeFenced
	}
	return nil
}

func (s *Service) replacementChatSource(ctx context.Context, expected domain.OrchestratorReplacement) (domain.OrchestratorReplacement, domain.SessionRecord, *Controller, error) {
	reader, ok := s.sessions.(replacementHibernationStore)
	if !ok {
		return expected, domain.SessionRecord{}, nil, domain.ErrReplacementStateInvalid
	}
	op, found, err := reader.GetOrchestratorReplacement(ctx, expected.ID)
	if err != nil {
		return op, domain.SessionRecord{}, nil, err
	}
	if !found || op.Revision != expected.Revision || op.Phase.Terminal() || op.SourceRetiredAt != nil || op.SourceSessionID != expected.SourceSessionID || op.SourceOwner != expected.SourceOwner || op.SourceCapabilityVerifier != expected.SourceCapabilityVerifier {
		return op, domain.SessionRecord{}, nil, domain.ErrReplacementSourceChanged
	}
	rec, err := s.requireChatSession(ctx, op.SourceSessionID)
	if err != nil {
		return op, rec, nil, err
	}
	if rec.IsTerminated || rec.ProjectID != op.ProjectID || rec.Kind != domain.KindOrchestrator || rec.Harness != domain.HarnessClaudeCode || rec.ControllerOwner() != op.SourceOwner || op.SourceCapabilityVerifier == "" || rec.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return op, rec, nil, domain.ErrReplacementSourceChanged
	}
	controller, err := s.Controller(rec.ID)
	if err != nil {
		return op, rec, nil, err
	}
	if controller.Generation() != op.SourceOwner.ControllerGeneration || controller.ProviderConversationID() == "" || controller.ProviderConversationID() != op.SourceOwner.ProviderConversationID {
		return op, rec, nil, domain.ErrReplacementSourceChanged
	}
	return op, rec, controller, nil
}

func (s *Service) replacementPreparationTurn(ctx context.Context, op domain.OrchestratorReplacement, controller *Controller) (*domain.ConversationTurn, error) {
	p := op.Preparation
	if p == nil || p.PromptIntentAt == nil {
		return nil, nil
	}
	return s.replacementControlTurn(ctx, controller, op.SourceSessionID, p.ControlClientID, p.PromptHash, p.ControlTurnID)
}

func (s *Service) replacementControlTurn(ctx context.Context, controller *Controller, session domain.SessionID, clientID, promptHash, turnID string) (*domain.ConversationTurn, error) {
	message, found, err := s.store.ConversationMessageByClientID(ctx, controller.ConversationID(), clientID)
	if err != nil || !found {
		return nil, err
	}
	if promptHash == "" || replacementPromptHash(message.Text) != promptHash || message.Role != domain.MessageRoleUser || message.Origin != domain.MessageOriginAutomation || message.SenderSessionID != "" || message.AuthoredByUser || message.DeliveryContentJSON != "" || message.Continuation {
		return nil, domain.ErrClientMessageConflict
	}
	turn, err := s.store.TurnByID(ctx, message.TurnID)
	if err != nil {
		return nil, err
	}
	if turn.HandledBySessionID != session || turn.ConversationID != controller.ConversationID() || turn.RolledBackAt != nil || turnID != "" && turn.ID != turnID {
		return nil, domain.ErrReplacementSourceChanged
	}
	return &turn, nil
}

// EnsureReplacementChatPreparation sends only under a persisted prompt intent.
// Lookup-only recovery never manufactures another turn after an uncertain send.
func (s *Service) EnsureReplacementChatPreparation(ctx context.Context, expected domain.OrchestratorReplacement, prompt string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	var delivery ports.ReplacementPreparationDelivery
	op, rec, controller, err := s.replacementChatSource(ctx, expected)
	if err != nil {
		return delivery, err
	}
	p := op.Preparation
	if op.Phase != domain.ReplacementPreparingHandoff || p == nil || p.PromptIntentAt == nil || p.PromptHash == "" || replacementPromptHash(prompt) != p.PromptHash || p.ControlClientID == "" {
		return delivery, domain.ErrReplacementStateInvalid
	}
	delivery.Owner, delivery.NativeID = op.SourceOwner, controller.ProviderConversationID()
	turn, err := s.replacementPreparationTurn(ctx, op, controller)
	if err != nil {
		return delivery, err
	}
	if turn != nil {
		delivery.TurnID = turn.ID
		return delivery, nil
	}
	if !allowNew || p.ControlTurnID != "" {
		delivery.Uncertain = true
		return delivery, nil
	}
	if controller.State() != ports.ChatControllerReady {
		return delivery, domain.ErrReplacementSourceBusy
	}
	ctx = context.WithValue(ctx, replacementControlContextKey{}, replacementControlGrant{session: rec.ID, generation: controller.Generation(), clientID: p.ControlClientID, promptHash: p.PromptHash})
	if _, err := controller.Send(ctx, ports.ChatUserMessage{Text: prompt, ClientMessageID: p.ControlClientID, Origin: domain.MessageOriginAutomation}); err != nil {
		return delivery, err
	}
	// The same durable client key recovers acceptance even when Send returns an
	// empty duplicate receipt. Recheck the owner before certifying that receipt.
	if _, _, _, err := s.replacementChatSource(ctx, op); err != nil {
		return delivery, err
	}
	turn, err = s.replacementPreparationTurn(ctx, op, controller)
	if err == nil && turn != nil {
		delivery.TurnID = turn.ID
	} else {
		delivery.Uncertain = true
	}
	return delivery, err
}

// ObserveReplacementChatSource reads controller facts and its exact control
// receipt. A completed child or unclassified provider turn cannot verify it.
func (s *Service) ObserveReplacementChatSource(ctx context.Context, expected domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	var observation ports.ReplacementSourceObservation
	op, _, controller, err := s.replacementChatSource(ctx, expected)
	if err != nil {
		return observation, err
	}
	observation.Owner, observation.NativeID, observation.Provenance = op.SourceOwner, controller.ProviderConversationID(), domain.NativeThreadMain
	controller.mu.Lock()
	observation.State = controller.state
	if controller.pendingTurnID != "" || controller.dispatchingTurnID != "" || controller.compactionPending || controller.handoff != controllerHandoffNone {
		observation.State = ports.ChatControllerBusy
	}
	controller.mu.Unlock()
	observation.PendingApproval, err = s.store.HasPendingConversationInteractions(ctx, controller.ConversationID())
	if err != nil {
		return observation, err
	}
	rows, err := s.reader.LoadConversationSnapshot(ctx, controller.ConversationID())
	if err != nil {
		return observation, err
	}
	for i := range rows.Turns {
		turn := &rows.Turns[i]
		if turn.HandledBySessionID == op.SourceSessionID && !turn.State.Terminal() && turn.State != domain.TurnStateQueued {
			observation.ActiveTurn = turn
			break
		}
	}
	control, err := s.replacementPreparationTurn(ctx, op, controller)
	if err != nil {
		return observation, err
	}
	if control != nil && control.State == domain.TurnStateCompleted {
		reader, ok := s.store.(nativeThreadEvidenceReader)
		if !ok {
			return observation, &ports.NativeTranscriptError{Code: "TRANSCRIPT_PROVENANCE_UNKNOWN", Reason: "Preparation native provenance is unavailable"}
		}
		evidence, err := reader.ConversationNativeThreadEvidence(ctx, control.ConversationID, op.SourceSessionID)
		if err != nil {
			return observation, err
		}
		branch, err := s.store.ConversationBranch(ctx, control.ConversationID, control.BranchID)
		if err != nil {
			return observation, err
		}
		if branch.ProviderConversationID != observation.NativeID || domain.IndexNativeThreadEvidence(evidence).Classify(control.BranchID, control.ProviderTurnID, observation.NativeID) != domain.NativeThreadMain {
			return observation, &ports.NativeTranscriptError{Code: "TRANSCRIPT_PROVENANCE_UNKNOWN", Reason: "Preparation turn is not verified on the source native main line"}
		}
	}
	observation.ControlTurn, observation.ObservedAt = control, s.now().UTC()
	return observation, nil
}
