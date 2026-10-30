package chat

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementHibernationStore interface {
	GetOrchestratorReplacement(context.Context, string) (domain.OrchestratorReplacement, bool, error)
	SetReplacementSessionHibernated(context.Context, string, int64, domain.SessionID, int64, time.Time) (bool, error)
}

// HibernateChatForReplacement parks only the operation-bound controller. The
// provider must confirm shutdown; the caller still binds its durable receipt
// before exporting evidence.
func (s *Service) HibernateChatForReplacement(ctx context.Context, id domain.SessionID, operation string) (bool, error) {
	store, ok := s.sessions.(replacementHibernationStore)
	if !ok {
		return false, errors.New("replacement hibernation store is unavailable")
	}
	op, found, err := store.GetOrchestratorReplacement(ctx, operation)
	if err != nil || !found {
		return false, err
	}
	rec, err := s.requireChatSession(ctx, id)
	if err != nil {
		return false, err
	}
	if !op.AllowsChatQuiescence(rec) {
		return false, nil
	}
	return s.hibernateChat(ctx, id, hibernateOptions{replacement: &op, ignoreViews: true})
}

// QuiesceReplacementChatSource issues a receipt only for a confirmed shutdown
// performed by this call. A restart gap with no controller remains uncertain.
func (s *Service) QuiesceReplacementChatSource(ctx context.Context, expected domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
	var stop domain.ReplacementControllerStop
	op, rec, _, err := s.replacementChatSource(ctx, expected)
	if err != nil {
		return stop, err
	}
	if op.Phase != domain.ReplacementExportingTranscript || !op.AllowsChatQuiescence(rec) {
		return stop, domain.ErrReplacementStateInvalid
	}
	return s.quiesceReplacementChatOwner(ctx, op, rec)
}

// QuiesceReplacementChatSuccessor parks only the settled, operation-bound
// initial controller. A missing controller/marker is never a shutdown receipt.
func (s *Service) QuiesceReplacementChatSuccessor(ctx context.Context, expected domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
	var zero domain.ReplacementControllerStop
	op, rec, _, err := s.replacementChatSuccessor(ctx, expected)
	if err != nil {
		return zero, err
	}
	if op.Phase != domain.ReplacementVerifyingResume || !op.AllowsChatQuiescence(rec) {
		return zero, domain.ErrReplacementStateInvalid
	}
	observation, err := s.ObserveReplacementChatSuccessor(ctx, op)
	if err != nil {
		return zero, err
	}
	if observation.State != ports.ChatControllerReady || observation.PendingApproval || observation.ActiveTurn != nil && !observation.ActiveTurn.State.Terminal() || observation.ControlTurn == nil || observation.ControlTurn.State != domain.TurnStateCompleted || op.Bootstrap.TurnID != observation.ControlTurn.ID {
		return zero, domain.ErrReplacementSourceBusy
	}
	return s.quiesceReplacementChatOwner(ctx, op, rec)
}

func (s *Service) quiesceReplacementChatOwner(ctx context.Context, op domain.OrchestratorReplacement, rec domain.SessionRecord) (domain.ReplacementControllerStop, error) {
	var stop domain.ReplacementControllerStop
	owner, verifier := rec.ControllerOwner(), rec.Metadata.BrowserCapabilityVerifier
	stopped, err := s.HibernateChatForReplacement(ctx, rec.ID, op.ID)
	if err != nil {
		return stop, err
	}
	if !stopped {
		if s.HasLiveChatController(rec.ID) {
			return stop, domain.ErrReplacementSourceBusy
		}
		return stop, ports.ErrChatRecoveryInconclusive
	}
	reader, ok := s.sessions.(replacementHibernationStore)
	if !ok {
		return stop, domain.ErrReplacementStateInvalid
	}
	current, found, err := reader.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		return stop, err
	}
	if !found || current.Revision != op.Revision || current.Phase != op.Phase || current.SourceOwner != op.SourceOwner || current.SourceCapabilityVerifier != op.SourceCapabilityVerifier {
		return stop, domain.ErrReplacementSourceChanged
	}
	rec, err = s.requireChatSession(ctx, rec.ID)
	if err != nil {
		return stop, err
	}
	stop = domain.ReplacementControllerStop{Owner: owner, CapabilityVerifier: verifier, NativeID: owner.ProviderConversationID, Method: "chat-hibernate", ConfirmedAt: s.now().UTC()}
	if s.HasLiveChatController(rec.ID) || !stop.MatchesSource(rec) {
		return domain.ReplacementControllerStop{}, ports.ErrChatRecoveryInconclusive
	}
	return stop, nil
}
