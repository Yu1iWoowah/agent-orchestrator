package chat

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// QueueReplacementIntake retains ordinary input without a provider controller,
// including a TUI source. The existing canonical queue owns payload and IDs.
func (s *Service) QueueReplacementIntake(ctx context.Context, id domain.SessionID, msg ports.ChatUserMessage) (domain.ConversationTurn, error) {
	var zero domain.ConversationTurn
	if s.sessions == nil {
		return zero, domain.ErrReplacementStateInvalid
	}
	rec, found, err := s.sessions.GetSession(ctx, id)
	if err != nil {
		return zero, err
	}
	if !found || rec.Kind != domain.KindOrchestrator {
		return zero, domain.ErrReplacementSourceChanged
	}
	if rec.IsTerminated {
		return s.queueRetiredReplacementIntake(ctx, rec, msg)
	}
	if err := s.replacementWakeFence(ctx, id); !errors.Is(err, domain.ErrReplacementIntakeFenced) {
		if err != nil {
			return zero, err
		}
		return zero, domain.ErrReplacementStateInvalid
	}
	if err := validateFileReferenceContent(msg, string(id)); err != nil {
		return zero, err
	}
	msg = s.resolveSteerSender(ctx, msg)
	turn, err := s.queueWithoutController(ctx, rec, msg)
	if err != nil || turn.ID != "" || msg.ClientMessageID == "" {
		return turn, err
	}
	// Ordinary provisioning callers keep their legacy empty duplicate receipt;
	// replacement intake can explicitly recover the original canonical turn.
	conversation, err := s.store.ConversationForSession(ctx, id)
	if err != nil {
		return zero, err
	}
	message, exists, err := s.store.ConversationMessageByClientID(ctx, conversation.ID, msg.ClientMessageID)
	if err != nil {
		return zero, err
	}
	if !exists {
		return zero, domain.ErrReplacementStateInvalid
	}
	return s.store.TurnByID(ctx, message.TurnID)
}

type retiredReplacementIntakeWriter interface {
	AppendRetiredReplacementIntake(context.Context, domain.SessionID, string, domain.ConversationMessage, string, time.Time) (domain.ConversationTurn, error)
}

func (s *Service) queueRetiredReplacementIntake(ctx context.Context, source domain.SessionRecord, msg ports.ChatUserMessage) (domain.ConversationTurn, error) {
	var zero domain.ConversationTurn
	reader, ok := s.sessions.(interface {
		GetRetiredOrchestratorReplacementBySource(context.Context, domain.SessionID) (domain.OrchestratorReplacement, bool, error)
	})
	if !ok {
		return zero, ErrNoController
	}
	op, found, err := reader.GetRetiredOrchestratorReplacementBySource(ctx, source.ID)
	if err != nil {
		return zero, err
	}
	if !found {
		return zero, ErrNoController
	}
	owner := op.SourceOwner
	owner.IsTerminated = true
	if op.ProjectID != source.ProjectID || source.ControllerOwner() != owner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return zero, domain.ErrReplacementSourceChanged
	}
	writer, ok := s.store.(retiredReplacementIntakeWriter)
	if !ok {
		return zero, domain.ErrReplacementIntakeFenced
	}
	if err := validateReplacementControlMessage(ctx, source.ID, source.Metadata.ControllerGeneration, msg); err != nil {
		return zero, err
	}
	if msg.Continuation {
		return zero, ErrContinuationUnavailable
	}
	if err := validateFileReferenceContent(msg, string(source.ID)); err != nil {
		return zero, err
	}
	msg = s.resolveSteerSender(ctx, msg)
	hash, err := clientPayloadHash(msg)
	if err != nil {
		return zero, err
	}
	var originalID string
	if len(msg.Excerpts) > 0 {
		// The old view may name the shared project row now bound to successor.
		// The writer verifies membership in the operation's source/project.
		originalID = msg.Excerpts[0].ConversationID
	} else {
		conversation, err := s.ensureConversation(ctx, source)
		if err != nil {
			return zero, err
		}
		originalID = conversation.ID
	}
	content, err := queuedDeliveryContent(msg, originalID)
	if err != nil {
		return zero, err
	}
	message := domain.ConversationMessage{ID: s.newID(), Text: msg.Text, Origin: normalizeOrigin(msg.Origin), ClientMessageID: msg.ClientMessageID, ClientPayloadHash: hash, DeliveryContentJSON: content, SenderSessionID: msg.SenderSessionID, SenderProjectID: msg.SenderProjectID, SenderDisplayName: msg.SenderDisplayName, AuthoredByUser: msg.AuthoredByUser, InteractionAt: msg.InteractionAt}
	turn, err := writer.AppendRetiredReplacementIntake(ctx, source.ID, originalID, message, s.newID(), s.now())
	if err == nil && turn.HandledBySessionID != source.ID {
		// Accepted canonical intake survives a missing/busy controller. Never wake
		// either source or successor to turn this receipt into delivery evidence.
		_ = s.DrainQueued(ctx, turn.HandledBySessionID)
	}
	return turn, err
}
