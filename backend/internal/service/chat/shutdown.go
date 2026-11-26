package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/telemetrymeta"
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
			return c.blocked("controller_busy", nil)
		case <-time.After(time.Millisecond):
		}
	}
	defer c.sendMu.Unlock()
	for !c.deliveryCheckMu.TryLock() {
		select {
		case <-checkCtx.Done():
			return c.blocked("controller_busy", nil)
		case <-time.After(time.Millisecond):
		}
	}
	defer c.deliveryCheckMu.Unlock()
	err := c.settleOutboundLocked(checkCtx, true)
	settled = err == nil
	return err
}

// Callers fence admission and hold sendMu. The same evidence applies to normal
// detach and managed update/hibernate; neither may manufacture a missing receipt.
func (c *Controller) settleOutboundLocked(ctx context.Context, notify bool) error {
	c.mu.Lock()
	compacting := c.compactionPending
	uncovered := c.settingsAttempt != nil && c.settingsAttempt.EffectPending || c.work[controllerProviderWork] > 0 || c.settingsAttemptRunning || c.dispatchingTurnID != ""
	nativeInFlight := c.pendingTurnID != "" || c.state == ports.ChatControllerBusy
	c.mu.Unlock()
	if uncovered {
		return c.blocked("provider_work_pending", nil)
	}
	if compacting {
		if !notify {
			return c.blocked("compaction_pending", nil)
		}
		if err := c.outboundAttention(ctx, false); err != nil {
			return c.blocked("compaction_pending", err)
		}
		reconciler, ok := c.conv.(ports.ChatOutboundReconciler)
		if !ok {
			return c.blocked("compaction_pending", nil)
		}
		err := reconciler.ReconcileOutbound(ctx)
		if err != nil {
			return c.blocked("compaction_pending", err)
		}
		c.clearOutboundAttention(ctx)
		c.mu.Lock()
		c.compactionPending = false
		c.mu.Unlock()
	}
	observer, supported := c.conv.(ports.ChatDeliveryObserver)
	journal, managed := c.store.(chatDeliveryJournal)
	if !managed || !supported {
		if nativeInFlight {
			return c.blocked("turn_in_flight", nil)
		}
		return nil
	}
	if c.reviewID != "" {
		if nativeInFlight {
			return c.blocked("turn_in_flight", nil)
		}
		_, err := c.flushOutbound(ctx, observer, nil, notify)
		if err != nil {
			c.warnFlushNotSettled(err, nil)
			reason, _ := flushReason(err)
			return c.blocked(reason, err)
		}
		return nil
	}
	pending, err := journal.PendingChatDeliveries(ctx, c.sessionID)
	if err != nil {
		return c.deliveryJournalBlocked("", err)
	}
	// An attempt from a host that no longer exists can never be answered by
	// the attached one: settle it first instead of failing the flush on it.
	keys := make([]ports.ChatDeliveryLookup, 0, len(pending))
	for _, a := range pending {
		settled, err := c.settleGoneHostDelivery(ctx, a)
		if err != nil {
			return c.deliveryJournalBlocked(a.ID, err)
		}
		if !settled {
			keys = append(keys, deliveryLookup(a))
		}
	}
	receipts, err := c.flushDeliveryReceipts(ctx, observer, keys, notify)
	if err != nil {
		return err
	}
	keysFilled := false
	for i, r := range receipts {
		filled, err := c.fillDeliveryLookup(&keys[i], r)
		if err != nil {
			return c.deliveryJournalBlocked(keys[i].AttemptID, err)
		}
		keysFilled = keysFilled || filled
	}
	if keysFilled {
		receipts, err = c.flushDeliveryReceipts(ctx, observer, keys, false)
		if err != nil {
			return err
		}
	}
	preserver, preserves := c.conv.(ports.ChatProviderPreserver)
	preserves = preserves && preserver.PreservesProviderOnClose()
	for i, r := range receipts {
		saved, err := c.commitDeliveryLookupReceipt(ctx, keys[i], r)
		if err != nil {
			return c.deliveryJournalBlocked(keys[i].AttemptID, err)
		}
		settled := saved.ExecutionSettled || saved.Disposition == "known_not_sent" || saved.Disposition == "accepted" && r.CustodyFlushed && preserves
		if !settled {
			return c.blocked("delivery_not_settled", nil)
		}
	}
	if len(keys) == 0 {
		c.mu.Lock()
		nativeInFlight = c.pendingTurnID != "" || c.state == ports.ChatControllerBusy
		c.mu.Unlock()
		if nativeInFlight {
			return c.blocked("turn_in_flight", nil)
		}
	}
	return nil
}

func (c *Controller) flushDeliveryReceipts(ctx context.Context, observer ports.ChatDeliveryObserver, keys []ports.ChatDeliveryLookup, notify bool) ([]domain.ChatDeliveryReceipt, error) {
	receipts, err := c.flushOutbound(ctx, observer, keys, notify)
	if err != nil {
		c.warnFlushNotSettled(err, keys)
		reason, _ := flushReason(err)
		return nil, c.blocked(reason, err)
	}
	if len(receipts) != len(keys) {
		return nil, c.blocked("delivery_receipt_missing", nil)
	}
	for i, key := range keys {
		if receipts[i].AttemptID != key.AttemptID {
			return nil, c.blocked("delivery_receipt_missing", nil)
		}
	}
	return receipts, nil
}

// blocked names this session and a fixed reason for a refused settlement.
func (c *Controller) blocked(reason string, cause error) error {
	return &ports.ChatShutdownBlockedError{SessionID: string(c.sessionID), Reason: reason, Cause: cause}
}

func (c *Controller) deliveryJournalBlocked(attempt string, cause error) error {
	logDeliveryJournalFailure(c.log, c.sessionID, attempt, cause)
	return c.blocked("delivery_journal_unavailable", cause)
}

func logDeliveryJournalFailure(log *slog.Logger, id domain.SessionID, attempt string, cause error) {
	if log == nil || cause == nil {
		return
	}
	stage := "reconcile"
	var journal *ports.ChatDeliveryJournalError
	if errors.As(cause, &journal) {
		stage, cause = journal.Stage, journal.Cause
		if attempt == "" {
			attempt = journal.AttemptID
		}
	} else if errors.Is(cause, ports.ErrAgentSettingsConflict) {
		stage = "version_or_owner"
	}
	attrs := []any{"session", id, "reason", "delivery_journal_unavailable", "stage", stage, "attempt", attempt}
	var decode *ports.ChatDeliveryDecodeError
	switch {
	case errors.As(cause, &decode):
		attrs = append(attrs, "error_type", fmt.Sprintf("%T", decode.Cause))
		var typeError *json.UnmarshalTypeError
		if errors.As(decode.Cause, &typeError) {
			attrs = append(attrs, "field", typeError.Field)
		}
	default:
		attrs = append(attrs, "error_type", fmt.Sprintf("%T", cause), "error_kind", telemetrymeta.ErrorType(cause))
		var coded interface{ Code() int }
		if errors.As(cause, &coded) {
			attrs = append(attrs, "sqlite_code", coded.Code())
		}
	}
	log.Warn("chat delivery journal unavailable", attrs...)
}

// Record an already-observed custody failure before a native read can consume
// the remaining shutdown budget. A successful read still requires another flush.
func (c *Controller) flushOutbound(ctx context.Context, observer ports.ChatDeliveryObserver, keys []ports.ChatDeliveryLookup, notify bool) ([]domain.ChatDeliveryReceipt, error) {
	receipts, err := observer.FenceAndFlushOutbound(ctx, keys)
	reconciler, canReconcile := c.conv.(ports.ChatOutboundReconciler)
	diagnose := notify && (canReconcile || len(keys) == 0)
	if err != nil && diagnose {
		if attentionErr := c.outboundAttention(ctx, false); attentionErr != nil {
			return nil, errors.Join(err, attentionErr)
		}
		if canReconcile {
			if queryErr := reconciler.ReconcileOutbound(ctx); queryErr != nil {
				return nil, errors.Join(err, queryErr)
			}
			receipts, err = observer.FenceAndFlushOutbound(ctx, keys)
		}
	}
	if err == nil && (canReconcile || len(keys) == 0) {
		c.mu.Lock()
		untrackedNative := len(keys) == 0 && (c.pendingTurnID != "" || c.compactionPending || c.state == ports.ChatControllerBusy)
		c.mu.Unlock()
		// Empty custody is not terminal evidence for untracked native work.
		if !untrackedNative {
			c.clearOutboundAttention(ctx)
		}
	}
	return receipts, err
}

// flushReason classifies a refused delivery flush without provider text: the
// host's named journal gap, its empty refusal (no record, journal checkpoint),
// or a host that did not answer.
func flushReason(err error) (string, []string) {
	var gap *ports.ChatDeliveryGapError
	switch {
	case errors.As(err, &gap):
		return "journal_gap", gap.Methods
	case errors.Is(err, ports.ErrChatDeliveryDenied):
		return "host_answered_without_record", nil
	case errors.Is(err, ports.ErrChatDeliveryUnverified):
		return "delivery_unverified", nil
	case errors.Is(err, context.DeadlineExceeded):
		return "flush_timeout", nil
	default:
		return "host_unreachable", nil
	}
}

// warnFlushNotSettled records why a flush was refused and the shape of the
// keys it carried (counts only), so a stale epoch is told apart from a gap.
func (c *Controller) warnFlushNotSettled(err error, keys []ports.ChatDeliveryLookup) {
	reason, methods := flushReason(err)
	current := ""
	if reporter, ok := c.conv.(ports.ChatDeliveryEpochReporter); ok {
		current = reporter.OutboundProviderEpoch()
	}
	withEpoch, currentEpoch, withoutRequest := 0, 0, 0
	for _, key := range keys {
		if key.ProviderEpoch != "" {
			withEpoch++
			if key.ProviderEpoch == current {
				currentEpoch++
			}
		}
		if key.ProviderRequestID == "" {
			withoutRequest++
		}
	}
	c.log.Warn("chat delivery flush not settled", "session", c.sessionID, "reason", reason, "methods", methods,
		"keys", len(keys), "keys_with_epoch", withEpoch, "keys_current_epoch", currentEpoch, "keys_without_request", withoutRequest)
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
	s.shutdownPreservedSides = nil
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
	if err := s.reconcilePreviousBootDeliveries(ctx, false); err != nil {
		var journal *ports.ChatDeliveryJournalError
		if errors.As(err, &journal) && journal.Stage == listSessionsJournalStage {
			logDeliveryJournalFailure(s.log, "", "", journal)
		}
		return err
	}
	if s.sides != nil {
		s.sides.mu.Lock()
		s.sides.quiescing = true
		uncovered := len(s.sides.restoring) > 0 || s.sides.opening > 0
		runtimes := make([]*sideRuntime, 0, len(s.sides.runtimes))
		for _, runtime := range s.sides.runtimes {
			runtimes = append(runtimes, runtime)
		}
		s.sides.mu.Unlock()
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		for _, runtime := range runtimes {
			runtime.mu.Lock()
			id := runtime.compactionID
			canCheck := !runtime.compactionDispatching && id != "" && runtime.outboundUnverified && runtime.activeTurnID == "" && !runtime.dispatchBlocked
			runtime.mu.Unlock()
			if canCheck {
				s.sides.reconcileCompaction(checkCtx, runtime, true)
			}
		}
		preserved, err := s.sides.preservedCompactionSides(checkCtx, "")
		if err != nil {
			return err
		}
		if store, ok := s.sides.store.(*memorySideStore); ok {
			for _, id := range store.unverifiedSides() {
				if !preserved[id] {
					uncovered = true
				}
			}
		}
		for _, runtime := range runtimes {
			runtime.mu.Lock()
			preserved := preserved[runtime.side.ID] && runtime.activeTurnID == "" && !runtime.compactionDispatching
			pending := !preserved && (runtime.activeTurnID != "" || runtime.compacting || runtime.compactionDispatching || runtime.outboundUnverified)
			uncovered = uncovered || pending
			runtime.mu.Unlock()
			if preserved {
				continue
			}
			if _, ok := s.store.(outboundAttentionStore); ok {
				if err := outboundAttention(checkCtx, s.store, runtime.side.SessionID, "side:"+runtime.side.ID, !pending, s.now()); err != nil {
					if pending {
						return errors.Join(ports.ErrChatShutdownNotSettled, err)
					}
					s.log.Warn("outbound attention cleanup failed", "side", runtime.side.ID, "code", "CHAT_ATTENTION_CLEANUP_FAILED")
				}
			}
		}
		if uncovered {
			return &ports.ChatShutdownBlockedError{Reason: "side_chat_pending"}
		}
		s.mu.Lock()
		s.shutdownPreservedSides = sortedPreservedSides(preserved)
		s.mu.Unlock()
	}
	if starting > 0 {
		return &ports.ChatShutdownBlockedError{Reason: "controller_starting"}
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
	s.log.Info("shutdown preflight settled", "historical_unknown_sides", s.ShutdownPreservedSides(), "new_unknown_count", 0)
	return nil
}
