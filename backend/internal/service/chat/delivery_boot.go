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
	journal, ok := s.store.(interruptedDeliveryStore)
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

func (s *Service) reconcileDetachedDeliveries(ctx context.Context, journal interruptedDeliveryStore, id domain.SessionID) error {
	gate := s.controllerGate(domain.SessionConversationOwner(id))
	if !gate.tryLock() {
		return nil
	}
	defer gate.unlock()
	if s.HasLiveChatController(id) {
		return nil
	}
	attempts, err := journal.PendingChatDeliveries(ctx, id)
	if err != nil {
		logDeliveryJournalFailure(s.log, id, "", err)
		return &ports.ChatShutdownBlockedError{SessionID: string(id), Reason: "delivery_journal_unavailable", Cause: err}
	}
	settled := make(map[string]bool)
	for _, a := range attempts {
		if a.Receipt == nil || !s.previousBootEpoch(a.Receipt.ProviderEpoch) {
			settled[a.ConversationID] = false
			continue
		}
		actor, err := s.settleInterruptedDelivery(ctx, journal, a)
		if err != nil {
			logDeliveryJournalFailure(s.log, id, a.ID, err)
			return &ports.ChatShutdownBlockedError{SessionID: string(id), Reason: "delivery_journal_unavailable", Cause: err}
		}
		if _, seen := settled[a.ConversationID]; !seen && actor.ControllerGeneration == a.Owner.ControllerGeneration {
			settled[a.ConversationID] = true
		}
	}
	for conversation, closed := range settled {
		if closed {
			if err := outboundAttention(ctx, s.store, id, "conversation:"+conversation, true, s.now()); err != nil {
				return err
			}
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
		if _, err := s.settleInterruptedDelivery(ctx, store, a); err != nil {
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

func (s *Service) settleInterruptedDelivery(ctx context.Context, journal interruptedDeliveryStore, a domain.ChatDeliveryAttempt) (domain.AgentSettingsOwner, error) {
	current, err := journal.SessionAgentSettings(ctx, a.SessionID)
	if err != nil {
		return domain.AgentSettingsOwner{}, err
	}
	owner, at := ownerForSettings(current), laterChatTime(s.now(), a.UpdatedAt)
	if unsettledAcceptance(a) {
		return owner, journal.SettlePreviousBootAcceptance(ctx, a, owner, at)
	}
	return owner, journal.SettleInterruptedUnknownDelivery(ctx, a, at)
}
