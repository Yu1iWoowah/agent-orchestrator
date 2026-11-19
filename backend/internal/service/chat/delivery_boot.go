package chat

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type previousBootDeliveryStore interface {
	chatDeliveryJournal
	SettlePreviousBootAcceptance(context.Context, domain.ChatDeliveryAttempt, domain.AgentSettingsOwner, time.Time) error
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
		return c.blocked("delivery_journal_unavailable", err)
	}
	for _, a := range attempts {
		if _, err := c.settlePreviousBootAcceptance(ctx, a); err != nil {
			return c.blocked("delivery_journal_unavailable", err)
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
		c.releasePreviousBootHold(ctx, attempt)
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
	c.releasePreviousBootHold(ctx, a)
	return true, nil
}

func (c *Controller) releasePreviousBootHold(ctx context.Context, a domain.ChatDeliveryAttempt) {
	c.mu.Lock()
	wasRunning := c.pendingTurnID == a.Receipt.ProviderTurnID
	if wasRunning {
		c.pendingTurnID = ""
		c.state = ports.ChatControllerReady
	}
	if c.ackedTurnID == a.Receipt.ProviderTurnID {
		c.ackedTurnID = ""
	}
	c.mu.Unlock()
	if wasRunning {
		c.reportActivity(ctx, domain.ActivityIdle, "chat.delivery.previous_boot", c.now())
	}
	c.releasePendingDelivery(a.ID)
}

// ReconcilePreviousBootDeliveries also handles sessions without a controller.
// A previous boot is proved by the saved receipt itself, not by starting a
// provider or treating an empty controller registry as successful settlement.
func (s *Service) ReconcilePreviousBootDeliveries(ctx context.Context) error {
	journal, ok := s.store.(previousBootDeliveryStore)
	sessions, canList := s.sessions.(interface {
		ListAllSessions(context.Context) ([]domain.SessionRecord, error)
	})
	if !ok || !canList || s.previousBootEpoch == nil {
		return nil
	}
	records, err := sessions.ListAllSessions(ctx)
	if err != nil {
		return &ports.ChatShutdownBlockedError{Reason: "delivery_journal_unavailable", Cause: err}
	}
	var failures []error
	for _, rec := range records {
		if rec.IsTerminated || rec.Harness != domain.HarnessCodex || rec.Mode != domain.SessionModeChat || s.HasLiveChatController(rec.ID) {
			continue
		}
		if err := s.reconcileDetachedAcceptance(ctx, journal, rec.ID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) reconcileDetachedAcceptance(ctx context.Context, journal previousBootDeliveryStore, id domain.SessionID) error {
	attempts, err := journal.PendingChatDeliveries(ctx, id)
	if err != nil {
		logDeliveryJournalFailure(s.log, id, "", err)
		return &ports.ChatShutdownBlockedError{SessionID: string(id), Reason: "delivery_journal_unavailable", Cause: err}
	}
	for _, a := range attempts {
		if !unsettledAcceptance(a) || !s.previousBootEpoch(a.Receipt.ProviderEpoch) {
			continue
		}
		current, err := journal.SessionAgentSettings(ctx, id)
		if err == nil {
			err = journal.SettlePreviousBootAcceptance(ctx, a, ownerForSettings(current), laterChatTime(s.now(), a.UpdatedAt))
		}
		if err != nil {
			logDeliveryJournalFailure(s.log, id, a.ID, err)
			return &ports.ChatShutdownBlockedError{SessionID: string(id), Reason: "delivery_journal_unavailable", Cause: err}
		}
	}
	return nil
}
