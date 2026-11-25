package chat

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const listSessionsJournalStage = "list_sessions"

type previousBootDeliveryStore interface {
	chatDeliveryJournal
	SettlePreviousBootAcceptance(context.Context, domain.ChatDeliveryAttempt, domain.AgentSettingsOwner, time.Time) error
}

type interruptedDeliveryStore interface {
	previousBootDeliveryStore
	SettleInterruptedUnknownDelivery(context.Context, domain.ChatDeliveryAttempt, time.Time) error
}

type detachedDeliveryStore interface {
	interruptedDeliveryStore
	SettleDetachedDeliveryWithAttention(context.Context, domain.ChatDeliveryAttempt, func(ctx context.Context, closeAttention bool) error) error
}

func (c *Controller) reconcileBeforeHibernate(ctx context.Context) error {
	journal, ok := c.store.(previousBootDeliveryStore)
	if !ok || !c.sendMu.TryLock() {
		return nil
	}
	defer c.sendMu.Unlock()
	if !c.deliveryCheckMu.TryLock() {
		return nil
	}
	defer c.deliveryCheckMu.Unlock()
	attempts, err := journal.PendingChatDeliveries(ctx, c.sessionID)
	if err != nil {
		return c.deliveryJournalBlocked("", err)
	}
	for _, a := range attempts {
		if _, err := c.settlePreviousBootAcceptance(ctx, a); err != nil {
			return c.deliveryJournalBlocked(a.ID, err)
		}
	}
	return nil
}

func unsettledAcceptance(a domain.ChatDeliveryAttempt) bool {
	return !a.ExecutionSettled && unsettledAcceptanceReceipt(a)
}

func unsettledAcceptanceReceipt(a domain.ChatDeliveryAttempt) bool {
	return a.Disposition == domain.ChatDeliveryAccepted &&
		a.Receipt != nil && !a.Receipt.ExecutionSettled && a.Receipt.ProviderTurnID != ""
}

func (c *Controller) settlePreviousBootAcceptance(ctx context.Context, attempt domain.ChatDeliveryAttempt) (bool, error) {
	if c.harness != domain.HarnessCodex || !unsettledAcceptance(attempt) || !c.goneHostEpoch(attempt) {
		return false, nil
	}
	journal, ok := c.store.(previousBootDeliveryStore)
	if !ok {
		return false, ports.ErrChatDeliveryUnverified
	}
	c.deliveryReceiptMu.Lock()
	defer c.deliveryReceiptMu.Unlock()
	a, err := journal.ChatDeliveryAttempt(ctx, c.sessionID, attempt.ID)
	if err != nil {
		return false, err
	}
	if a.ExecutionSettled {
		c.releaseInterruptedDeliveryHold(ctx, attempt)
		return true, nil
	}
	if !unsettledAcceptance(a) || !c.goneHostEpoch(a) {
		return false, nil
	}
	current, err := c.deliveryOwner(ctx, journal)
	if err != nil {
		return false, err
	}
	if err := journal.SettlePreviousBootAcceptance(ctx, a, ownerForSettings(current), laterChatTime(c.now(), a.UpdatedAt)); err != nil {
		return false, err
	}
	c.releaseInterruptedDeliveryHold(ctx, a)
	return true, nil
}

func (c *Controller) releaseInterruptedDeliveryHold(ctx context.Context, a domain.ChatDeliveryAttempt) {
	providerTurn := a.PreparedTurnID
	if a.Receipt != nil && a.Receipt.ProviderTurnID != "" {
		providerTurn = a.Receipt.ProviderTurnID
	}
	c.mu.Lock()
	wasRunning := providerTurn != "" && c.pendingTurnID == providerTurn && c.state != ports.ChatControllerStopped
	if wasRunning {
		c.pendingTurnID = ""
		c.state = ports.ChatControllerReady
	}
	if c.ackedTurnID == providerTurn {
		c.ackedTurnID = ""
	}
	c.mu.Unlock()
	if wasRunning {
		c.reportActivity(ctx, domain.ActivityIdle, "chat.delivery.interrupted", c.now())
	}
	c.releasePendingDelivery(a.ID)
}

// ReconcilePreviousBootDeliveries also handles sessions without a controller.
// A previous boot is proved by the saved receipt itself, not by starting a
// provider or treating an empty controller registry as successful settlement.
func (s *Service) ReconcilePreviousBootDeliveries(ctx context.Context) error {
	journal, ok := s.store.(detachedDeliveryStore)
	sessions, canList := s.sessions.(interface {
		ListAllSessions(context.Context) ([]domain.SessionRecord, error)
	})
	if !ok || !canList || s.previousBootEpoch == nil {
		return nil
	}
	records, err := sessions.ListAllSessions(ctx)
	if err != nil {
		return &ports.ChatShutdownBlockedError{Reason: "delivery_journal_unavailable", Cause: &ports.ChatDeliveryJournalError{Stage: listSessionsJournalStage, Cause: err}}
	}
	var failures []error
	for _, rec := range records {
		if rec.IsTerminated || rec.Mode != domain.SessionModeChat {
			continue
		}
		if err := s.reconcileDetachedDeliveries(ctx, journal, rec.ID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) reconcileDetachedDeliveries(ctx context.Context, journal detachedDeliveryStore, id domain.SessionID) error {
	gate := s.controllerGate(domain.SessionConversationOwner(id))
	if !gate.tryLock() {
		return nil
	}
	defer gate.unlock()
	if s.HasLiveChatController(id) {
		return nil
	}
	blocked := func(attempt string, err error) error {
		logDeliveryJournalFailure(s.log, id, attempt, err)
		return &ports.ChatShutdownBlockedError{SessionID: string(id), Reason: "delivery_journal_unavailable", Cause: err}
	}
	attempts, err := journal.PendingChatDeliveries(ctx, id)
	if err != nil {
		return blocked("", err)
	}
	if len(attempts) == 0 {
		return nil
	}
	current, err := journal.SessionAgentSettings(ctx, id)
	if err != nil {
		return blocked("", err)
	}
	// Keep the final same-generation attempt pending until the question can
	// close with it. A conversation with any unverified attempt stays open.
	closeWith := make(map[string]string)
	eligible := attempts[:0]
	for _, a := range attempts {
		if a.Receipt == nil || !s.previousBootEpoch(a.Receipt.ProviderEpoch) {
			closeWith[a.ConversationID] = ""
			continue
		}
		eligible = append(eligible, a)
		if candidate, seen := closeWith[a.ConversationID]; (!seen || candidate != "") && current.OwnerGeneration == a.Owner.ControllerGeneration {
			closeWith[a.ConversationID] = a.ID
		}
	}
	var final []domain.ChatDeliveryAttempt
	for _, a := range eligible {
		if closeWith[a.ConversationID] == a.ID {
			final = append(final, a)
			continue
		}
		if err := s.settleInterruptedDelivery(ctx, journal, a); err != nil {
			return blocked(a.ID, err)
		}
	}
	for _, a := range final {
		err := journal.SettleDetachedDeliveryWithAttention(ctx, a, func(txCtx context.Context, closeAttention bool) error {
			if err := s.settleInterruptedDelivery(txCtx, journal, a); err != nil || !closeAttention {
				return err
			}
			return outboundAttention(txCtx, s.store, id, "conversation:"+a.ConversationID, true, s.now())
		})
		if err != nil {
			return blocked(a.ID, err)
		}
	}
	return nil
}

// ReconcileExitedAdapter keeps the original turn cleanup independent of
// delivery: the adapter may exit before its provider process group ends.
func (s *Service) ReconcileExitedAdapter(ctx context.Context, id domain.SessionID, conversation, branch, generation string, ended bool) (bool, error) {
	store, ok := s.store.(interface {
		interruptedDeliveryStore
		SettleExitedChatAdapter(context.Context, domain.SessionID, string, string, string, time.Time) error
	})
	if !ok {
		return false, ports.ErrChatDeliveryUnverified
	}
	if !ended {
		return false, store.SettleExitedChatAdapter(ctx, id, conversation, branch, generation, s.now())
	}

	gate := s.controllerGate(domain.SessionConversationOwner(id))
	if !gate.tryLock() {
		return false, nil
	}
	defer gate.unlock()
	session, found, err := s.sessions.GetSession(ctx, id)
	if err != nil {
		return false, err
	}
	if !found || session.IsTerminated || session.Mode != domain.SessionModeChat {
		return true, nil
	}
	controller, _ := s.Controller(id)
	if controller != nil {
		if !controller.sendMu.TryLock() {
			return false, nil
		}
		defer controller.sendMu.Unlock()
		controller.deliveryReceiptMu.Lock()
		defer controller.deliveryReceiptMu.Unlock()
	}

	attempts, err := store.PendingChatDeliveries(ctx, id)
	if err != nil {
		return false, err
	}
	unsettled := false
	for _, a := range attempts {
		if a.ConversationID != conversation {
			continue
		}
		if a.Owner.BranchID != branch || a.Owner.ControllerGeneration != generation {
			unsettled = true
			continue
		}
		if err := s.settleInterruptedDelivery(ctx, store, a); err != nil {
			return false, err
		}
		if controller != nil {
			controller.releaseInterruptedDeliveryHold(ctx, a)
		}
	}
	if err := store.SettleExitedChatAdapter(ctx, id, conversation, branch, generation, s.now()); err != nil {
		return false, err
	}
	// Outbound questions identify the conversation, so an older launch
	// cannot close a question that may belong to its successor.
	if session.ControllerOwner().ControllerGeneration != generation {
		return true, nil
	}
	if unsettled {
		return false, nil
	}
	err = outboundAttention(ctx, s.store, id, "conversation:"+conversation, true, s.now())
	return err == nil, err
}

func (s *Service) settleInterruptedDelivery(ctx context.Context, journal interruptedDeliveryStore, a domain.ChatDeliveryAttempt) error {
	current, err := journal.SessionAgentSettings(ctx, a.SessionID)
	if err != nil {
		return err
	}
	owner, at := ownerForSettings(current), laterChatTime(s.now(), a.UpdatedAt)
	if unsettledAcceptance(a) {
		return journal.SettlePreviousBootAcceptance(ctx, a, owner, at)
	}
	return journal.SettleInterruptedUnknownDelivery(ctx, a, at)
}
