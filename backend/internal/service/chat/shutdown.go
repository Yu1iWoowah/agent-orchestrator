package chat

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (c *Controller) outboundQuiescing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deliveryQuiescing
}
func (c *Controller) setOutboundQuiescing(value bool) {
	c.mu.Lock()
	c.deliveryQuiescing = value
	c.mu.Unlock()
	c.wakeDeliveryMonitor(false)
}

// PreflightShutdown runs before detaching any transport. Failure keeps the
// original owner and provider available for query/control and a later retry.
func (c *Controller) PreflightShutdown(ctx context.Context) error {
	c.setOutboundQuiescing(true)
	settled := false
	defer func() {
		if !settled {
			c.setOutboundQuiescing(false)
		}
	}()
	checkCtx, cancel := c.deadlineContext(ctx, 10*time.Second)
	defer cancel()

	// Native writers take sendMu; the read-only monitor never does. Use this
	// order in both shutdown and hibernate so neither can wait on the other.
	for !c.sendMu.TryLock() {
		select {
		case <-checkCtx.Done():
			return ports.ErrChatShutdownNotSettled
		case <-time.After(time.Millisecond):
		}
	}
	defer c.sendMu.Unlock()
	for !c.deliveryCheckMu.TryLock() {
		select {
		case <-checkCtx.Done():
			return ports.ErrChatShutdownNotSettled
		case <-time.After(time.Millisecond):
		}
	}
	defer c.deliveryCheckMu.Unlock()
	err := c.settleOutboundLocked(checkCtx)
	settled = err == nil
	return err
}

// Callers fence admission and hold sendMu. The same evidence applies to normal
// detach and managed update/hibernate; neither may manufacture a missing receipt.
func (c *Controller) settleOutboundLocked(ctx context.Context) error {
	c.mu.Lock()
	uncovered := c.settingsAttempt != nil && c.settingsAttempt.EffectPending || c.compactionPending || c.work[controllerProviderWork] > 0 || c.settingsAttemptRunning || c.dispatchingTurnID != ""
	nativeInFlight := c.pendingTurnID != "" || c.state == ports.ChatControllerBusy
	c.mu.Unlock()
	if uncovered {
		return ports.ErrChatShutdownNotSettled
	}
	observer, supported := c.conv.(ports.ChatDeliveryObserver)
	journal, managed := c.store.(chatDeliveryJournal)
	if !managed || !supported {
		if nativeInFlight {
			return ports.ErrChatShutdownNotSettled
		}
		return nil
	}
	if c.reviewID != "" {
		if nativeInFlight {
			return ports.ErrChatShutdownNotSettled
		}
		_, err := observer.FenceAndFlushOutbound(ctx, nil)
		if err != nil {
			return errors.Join(ports.ErrChatShutdownNotSettled, err)
		}
		return nil
	}
	attempts, err := journal.PendingChatDeliveries(ctx, c.sessionID)
	if err != nil {
		return errors.Join(ports.ErrChatShutdownNotSettled, err)
	}
	keys := make([]ports.ChatDeliveryLookup, 0, len(attempts))
	for _, a := range attempts {
		keys = append(keys, deliveryLookup(a))
	}
	receipts, err := observer.FenceAndFlushOutbound(ctx, keys)
	if err != nil || len(receipts) != len(keys) {
		return ports.ErrChatShutdownNotSettled
	}
	preserver, preserves := c.conv.(ports.ChatProviderPreserver)
	preserves = preserves && preserver.PreservesProviderOnClose()
	for i, r := range receipts {
		if r.AttemptID != attempts[i].ID {
			return ports.ErrChatShutdownNotSettled
		}
		saved, err := c.recordDeliveryReceipt(ctx, r)
		if err != nil {
			return errors.Join(ports.ErrChatShutdownNotSettled, err)
		}
		settled := saved.ExecutionSettled || saved.Disposition == "known_not_sent" || saved.Disposition == "accepted" && r.CustodyFlushed && preserves
		if !settled {
			return ports.ErrChatShutdownNotSettled
		}
	}
	if nativeInFlight && len(attempts) == 0 {
		return ports.ErrChatShutdownNotSettled
	}
	return nil
}

// PreflightShutdown fences every controller before checking any participant.
// Side paths without a durable delivery record are an explicit coverage gap;
// they cannot be detached while work or restore is in flight.
func (s *Service) PreflightShutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.shutdownPrepared && s.shutdownQuiescing {
		s.mu.Unlock()
		return nil
	}
	s.shutdownQuiescing = true
	starting := s.startingControllers
	controllers := make([]*Controller, 0, len(s.ownerControllers))
	for _, c := range s.ownerControllers {
		controllers = append(controllers, c)
	}
	s.mu.Unlock()
	for _, c := range controllers {
		c.setOutboundQuiescing(true)
	}
	success := false
	defer func() {
		if !success {
			s.mu.Lock()
			s.shutdownQuiescing = false
			s.shutdownPrepared = false
			s.mu.Unlock()
			for _, c := range controllers {
				c.setOutboundQuiescing(false)
			}
			if s.sides != nil {
				s.sides.mu.Lock()
				s.sides.quiescing = false
				s.sides.mu.Unlock()
				s.sides.signal()
			}
		}
	}()
	if s.sides != nil {
		s.sides.mu.Lock()
		s.sides.quiescing = true
		uncovered := len(s.sides.restoring) > 0 || s.sides.opening > 0
		for _, runtime := range s.sides.runtimes {
			runtime.mu.Lock()
			uncovered = uncovered || runtime.activeTurnID != "" || runtime.compacting || runtime.outboundUnverified
			runtime.mu.Unlock()
		}
		if store, ok := s.sides.store.(*memorySideStore); ok {
			for _, id := range store.unverifiedSides() {
				if s.sides.runtimes[id] == nil {
					uncovered = true
				}
			}
		}
		s.sides.mu.Unlock()
		if uncovered {
			return ports.ErrChatShutdownNotSettled
		}
	}
	if starting > 0 {
		return ports.ErrChatShutdownNotSettled
	}
	for _, c := range controllers {
		if err := c.PreflightShutdown(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.shutdownPrepared = true
	s.mu.Unlock()
	success = true
	return nil
}
