package chat

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type chatDeliveryJournal interface {
	agentSettingsContextReader
	ReserveChatDelivery(context.Context, domain.ChatDeliveryAttempt) (domain.ChatDeliveryAttempt, bool, error)
	UpdateChatDelivery(context.Context, domain.ChatDeliveryAttempt, int64, domain.AgentSettingsOwner) (domain.ChatDeliveryAttempt, error)
	ChatDeliveryAttempt(context.Context, domain.SessionID, string) (domain.ChatDeliveryAttempt, error)
	ChatDeliveryAttemptForTurn(context.Context, domain.SessionID, string) (domain.ChatDeliveryAttempt, error)
	PendingChatDeliveries(context.Context, domain.SessionID) ([]domain.ChatDeliveryAttempt, error)
}

func (c *Controller) deliveryOwner(ctx context.Context, journal chatDeliveryJournal) (domain.SessionAgentSettings, error) {
	current, err := journal.SessionAgentSettings(ctx, c.sessionID)
	if err != nil {
		return current, err
	}
	expected := c.settingsObservationOwner
	if current.IsTerminated || current.OwnerGeneration != expected.ControllerGeneration || current.AccountID != expected.AccountID || current.BranchID != expected.BranchID || current.ProviderConversationID != "" && current.ProviderConversationID != c.ProviderConversationID() {
		return current, ports.ErrAgentSettingsConflict
	}
	return current, nil
}

func (c *Controller) reserveDeliveryLocked(ctx context.Context, members []string) (*domain.ChatDeliveryAttempt, bool, error) {
	journal, ok := c.store.(chatDeliveryJournal)
	_, supported := c.conv.(ports.ChatDeliveryObserver)
	if !ok || !supported || c.reviewID != "" {
		return nil, true, nil
	}
	current, err := c.deliveryOwner(ctx, journal)
	if err != nil {
		return nil, false, err
	}
	at := c.now()
	nextCheck := at.Add(30 * time.Second)
	a := domain.ChatDeliveryAttempt{
		NextCheckAt: &nextCheck,
		ID:          uuid.NewString(), SessionID: c.sessionID, ConversationID: c.conversation.ID,
		Owner: ownerForSettings(current), Revision: current.Revision, TurnIDs: members,
		Disposition: domain.ChatDeliveryReserved, CreatedAt: at, UpdatedAt: at, LastProgressAt: at,
	}
	saved, created, err := journal.ReserveChatDelivery(ctx, a)
	if err == nil && created {
		c.startDeliveryMonitor()
	}
	return &saved, created, err
}

func (c *Controller) bindDeliveryPrepared(ctx context.Context, a domain.ChatDeliveryAttempt, prepared string) (domain.ChatDeliveryAttempt, error) {
	journal, ok := c.store.(chatDeliveryJournal)
	if !ok {
		return a, ports.ErrChatDeliveryUnverified
	}
	current, err := c.deliveryOwner(ctx, journal)
	if err != nil {
		return a, err
	}
	a.Version++
	a.PreparedTurnID, a.UpdatedAt = prepared, laterChatTime(c.now(), a.UpdatedAt)
	return journal.UpdateChatDelivery(ctx, a, a.Version-1, ownerForSettings(current))
}

// A native completion must not disappear when a read-only monitor commits the
// same row first. Each retry requalifies the owner and the exact receipt keys.
func (c *Controller) recordDeliveryReceipt(ctx context.Context, r domain.ChatDeliveryReceipt) (domain.ChatDeliveryAttempt, error) {
	c.deliveryReceiptMu.Lock()
	defer c.deliveryReceiptMu.Unlock()
	var saved domain.ChatDeliveryAttempt
	var err error
	for retry := 0; retry < 3; retry++ {
		saved, err = c.recordDeliveryReceiptOnce(ctx, r)
		if !errors.Is(err, ports.ErrAgentSettingsConflict) || ctx.Err() != nil {
			return saved, err
		}
	}
	return saved, err
}

func (c *Controller) recordDeliveryReceiptOnce(ctx context.Context, r domain.ChatDeliveryReceipt) (domain.ChatDeliveryAttempt, error) {
	journal, ok := c.store.(chatDeliveryJournal)
	if !ok || r.Validate() != nil {
		return domain.ChatDeliveryAttempt{}, ports.ErrChatDeliveryUnverified
	}
	current, err := c.deliveryOwner(ctx, journal)
	if err != nil {
		return domain.ChatDeliveryAttempt{}, err
	}
	a, err := journal.ChatDeliveryAttempt(ctx, c.sessionID, r.AttemptID)
	if err != nil {
		return a, err
	}
	if !a.Accepts(r) {
		return a, ports.ErrChatDeliveryUnverified
	}
	if a.ExecutionSettled && r.ExecutionSettled && r.Disposition == domain.ChatDeliveryAccepted && a.AttentionID == "" && a.BlockedSince == nil && a.ErrorCode == "" {
		return a, nil
	}
	previous := a.Receipt
	advanced := previous == nil || r.ProgressSequence > previous.ProgressSequence || r.ObservedAt.After(previous.ObservedAt)
	if previous != nil && r.ProgressSequence < previous.ProgressSequence {
		return a, nil
	}
	r.ExecutionSettled = r.ExecutionSettled || a.ExecutionSettled
	a.Version++
	a.UpdatedAt, a.Disposition, a.Receipt = laterChatTime(c.now(), a.UpdatedAt), r.Disposition, &r
	if r.Disposition == domain.ChatDeliveryAccepted {
		a.EvidenceLevel = 2
	}
	// Native progress is not acceptance. It only advances the silence deadline.
	nativeProgress := r.ProviderEpoch != "" && r.ProviderRequestID != "" && r.Progress != "" && r.ProgressSequence > 0
	if advanced && (r.Disposition == domain.ChatDeliveryAccepted || nativeProgress) {
		a.LastProgressAt = laterChatTime(c.now(), a.LastProgressAt)
		a.BlockedSince, a.ErrorCode = nil, ""
		nextCheck := a.LastProgressAt.Add(30 * time.Second)
		a.NextCheckAt = &nextCheck
	}
	a.ExecutionSettled = a.ExecutionSettled || r.ExecutionSettled
	if a.ExecutionSettled || r.Disposition == domain.ChatDeliveryNotSent {
		a.NextCheckAt, a.BlockedSince, a.ErrorCode = nil, nil, ""
	}
	saved, err := journal.UpdateChatDelivery(ctx, a, a.Version-1, ownerForSettings(current))
	if err == nil {
		if saved.ExecutionSettled && r.ProviderEpoch != "" {
			if acknowledger, ok := c.conv.(ports.ChatDeliveryAcknowledger); ok {
				ackCtx, cancel := c.deadlineContext(ctx, 5*time.Second)
				ackErr := acknowledger.AcknowledgeDeliveryAttempt(ackCtx, deliveryLookup(saved))
				cancel()
				if ackErr != nil {
					c.log.Warn("settled delivery evidence retained by host", "session", c.sessionID, "attempt", saved.ID)
				}
			}
		}
		resumed := false
		c.mu.Lock()
		if c.pendingDeliveryID == a.ID && (saved.ExecutionSettled || saved.Disposition == domain.ChatDeliveryNotSent) {
			resumed = true
			c.pendingDeliveryID = ""
		}
		c.mu.Unlock()
		c.wakeDeliveryMonitor(false)
		if resumed && !c.outboundQuiescing() {
			c.queueSettingsRecheck()
		}
	}
	return saved, err
}

func deliveryLookup(a domain.ChatDeliveryAttempt) ports.ChatDeliveryLookup {
	lookup := ports.ChatDeliveryLookup{AttemptID: a.ID, PreparedTurnID: a.PreparedTurnID}
	if a.Receipt != nil {
		lookup.ProviderEpoch = a.Receipt.ProviderEpoch
		lookup.ProviderRequestID = a.Receipt.ProviderRequestID
	}
	return lookup
}

func (c *Controller) recordLocalNotForwarded(ctx context.Context, a *domain.ChatDeliveryAttempt, prepared string) error {
	if a == nil {
		return nil
	}
	_, err := c.recordDeliveryReceipt(ctx, domain.ChatDeliveryReceipt{AttemptID: a.ID, ProviderTurnID: prepared, ProviderRequestID: "local:" + a.ID, Disposition: domain.ChatDeliveryNotSent, Source: "local_not_forwarded", Complete: true, ObservedAt: c.now()})
	return err
}

func (c *Controller) queryDelivery(ctx context.Context, a domain.ChatDeliveryAttempt) (domain.ChatDeliveryAttempt, error) {
	observer, ok := c.conv.(ports.ChatDeliveryObserver)
	if !ok {
		return a, ports.ErrChatDeliveryUnverified
	}
	queryCtx, cancel := c.deadlineContext(ctx, 10*time.Second)
	defer cancel()
	receipt, err := observer.QueryDeliveryAttempt(queryCtx, deliveryLookup(a))
	if err != nil {
		if errors.Is(context.Cause(queryCtx), context.DeadlineExceeded) {
			return a, errors.Join(ports.ErrChatDeliveryUnverified, context.DeadlineExceeded)
		}
		return a, ports.ErrChatDeliveryUnverified
	}
	if receipt.AttemptID != a.ID {
		return a, ports.ErrChatDeliveryUnverified
	}
	return c.recordDeliveryReceipt(ctx, receipt)
}

type deliveryAttentionWriter interface {
	CreateAttentionQuestion(context.Context, domain.AttentionQuestionCreate, time.Time) (domain.AttentionQuestionRecord, bool, error)
}

func (c *Controller) blockDelivery(ctx context.Context, a domain.ChatDeliveryAttempt, cause error) error {
	c.deliveryReceiptMu.Lock()
	defer c.deliveryReceiptMu.Unlock()
	journal, ok := c.store.(chatDeliveryJournal)
	if !ok {
		return ports.ErrChatDeliveryUnverified
	}
	current, err := c.deliveryOwner(ctx, journal)
	if err != nil {
		return err
	}
	a, err = journal.ChatDeliveryAttempt(ctx, c.sessionID, a.ID)
	if err != nil || a.ExecutionSettled || a.Disposition == domain.ChatDeliveryNotSent {
		return err
	}
	if a.Disposition == domain.ChatDeliveryReserved {
		r := domain.ChatDeliveryReceipt{AttemptID: a.ID, Disposition: domain.ChatDeliveryUnknown, Source: "incomplete", ObservedAt: c.now()}
		if a.Receipt != nil {
			r.ProviderEpoch, r.ProviderRequestID, r.ProviderTurnID = a.Receipt.ProviderEpoch, a.Receipt.ProviderRequestID, a.Receipt.ProviderTurnID
		}
		a.Receipt, a.Disposition = &r, domain.ChatDeliveryUnknown
	}
	if a.BlockedSince == nil {
		at := c.now()
		a.BlockedSince = &at
	}
	a.NextCheckAt = nil
	a.ErrorCode = "CHAT_EXECUTION_STATE_UNVERIFIED"
	if errors.Is(cause, context.DeadlineExceeded) {
		a.ErrorCode = "CHAT_DELIVERY_QUERY_TIMEOUT"
	}
	a.Version++
	a.UpdatedAt = c.now()
	a, err = journal.UpdateChatDelivery(ctx, a, a.Version-1, ownerForSettings(current))
	if err != nil {
		return err
	}
	attention, ok := c.store.(deliveryAttentionWriter)
	if !ok || a.AttentionID != "" {
		return nil
	}
	payload, err := json.Marshal(domain.AttentionQuestionPayload{SchemaVersion: 1,
		Title: "訊息執行狀態待確認", Explanation: "輸入與原執行紀錄已保留。尚未取得足夠證據，AO 不會自動重送；請檢查原執行或等待連線恢復。",
		Questions: []domain.AttentionPrompt{{Key: "execution", Header: "執行待確認", Question: "此輸入仍保留，請確認你已看到執行狀態待確認的提示。", Options: []domain.AttentionQuestionOption{
			{ID: "inspect", Label: "我會檢查原執行", Description: "保留原紀錄，不建立新的執行。", Recommended: true},
			{ID: "keep", Label: "保持保留", Description: "等待原執行的證據或連線恢復，不自動重送。"},
		}}},
	})
	if err != nil {
		return err
	}
	q, _, err := attention.CreateAttentionQuestion(ctx, domain.AttentionQuestionCreate{ID: uuid.NewString(), OriginSessionID: c.sessionID, ClientKey: "delivery-" + a.ID, Audience: "user", Kind: domain.AttentionQuestionKindQuestion, Payload: payload}, c.now())
	if err != nil {
		return err
	}
	a.AttentionID, a.UpdatedAt = q.ID, c.now()
	a.Version++
	_, err = journal.UpdateChatDelivery(ctx, a, a.Version-1, ownerForSettings(current))
	return err
}

func (c *Controller) startDeliveryMonitor() {
	if _, ok := c.conv.(ports.ChatDeliveryObserver); !ok || c.reviewID != "" {
		return
	}
	if _, ok := c.store.(chatDeliveryJournal); !ok || c.reviewID != "" {
		return
	}
	c.mu.Lock()
	if c.deliveryMonitorStarted {
		wake := c.deliveryWake
		c.mu.Unlock()
		select {
		case wake <- struct{}{}:
		default:
		}
		return
	}
	c.deliveryMonitorStarted = true
	c.deliveryImmediate = false
	c.deliveryWake = make(chan struct{}, 1)
	c.mu.Unlock()
	go c.monitorDeliveries()
}

func (c *Controller) wakeDeliveryMonitor(immediate bool) {
	c.mu.Lock()
	c.deliveryImmediate = c.deliveryImmediate || immediate
	wake := c.deliveryWake
	c.mu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (c *Controller) monitorDeliveries() {
	journal, ok := c.store.(chatDeliveryJournal)
	if !ok {
		return
	}
	firstSweep := true
	for {
		c.mu.Lock()
		force, paused := c.deliveryImmediate, c.deliveryQuiescing
		c.deliveryImmediate = false
		wake := c.deliveryWake
		c.mu.Unlock()
		var timer <-chan time.Time
		if !paused {
			c.deliveryCheckMu.Lock()
			if c.outboundQuiescing() {
				c.deliveryCheckMu.Unlock()
				continue
			}
			ctx, cancel := c.deadlineContext(context.Background(), 10*time.Second)
			attempts, err := journal.PendingChatDeliveries(ctx, c.sessionID)
			cancel()
			if err != nil {
				timer = c.timerAfter(30 * time.Second)
			} else {
				var earliest time.Time
				for _, a := range attempts {
					if a.BlockedSince != nil && !force && !firstSweep && a.AttentionID != "" {
						continue
					}
					due := a.LastProgressAt.Add(30 * time.Second)
					if a.NextCheckAt != nil {
						due = *a.NextCheckAt
					}
					if force || firstSweep && a.BlockedSince != nil || !due.After(c.now()) {
						queryCtx, queryCancel := c.deadlineContext(context.Background(), 10*time.Second)
						saved, queryErr := c.queryDelivery(queryCtx, a)
						queryCancel()
						c.mu.Lock()
						quiescing := c.deliveryQuiescing
						c.mu.Unlock()
						if quiescing {
							break
						}
						if queryErr == nil && (saved.ExecutionSettled || saved.Disposition == domain.ChatDeliveryNotSent) {
							continue
						}
						if queryErr == nil && saved.LastProgressAt.After(a.LastProgressAt) {
							due = saved.LastProgressAt.Add(30 * time.Second)
						} else if !due.After(c.now()) || a.BlockedSince != nil {
							blockCtx, blockCancel := c.deadlineContext(context.Background(), 5*time.Second)
							blockErr := c.blockDelivery(blockCtx, a, queryErr)
							blockCancel()
							if blockErr == nil {
								continue
							}
							c.log.Warn("delivery visibility checkpoint unavailable", "session", c.sessionID, "attempt", a.ID, "code", "CHAT_DELIVERY_DIAGNOSIS_UNAVAILABLE")
							due = c.now().Add(5 * time.Second)
						}
					}
					if earliest.IsZero() || due.Before(earliest) {
						earliest = due
					}
				}
				if !earliest.IsZero() {
					timer = c.timerAfter(max(time.Millisecond, earliest.Sub(c.now())))
				}
			}

			firstSweep = false
			c.deliveryCheckMu.Unlock()
		}
		select {
		case <-c.stopped:
			return
		case <-wake:
		case <-timer:
		}
	}
}

func laterChatTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

// Retry durable intake without requiring a second user send. Coalesce failures
// and back off while storage is unavailable; stopped controllers own no timer.
func (c *Controller) scheduleDeliveryRetry() {
	c.mu.Lock()
	if c.deliveryRetryScheduled {
		c.mu.Unlock()
		return
	}
	switch c.deliveryRetryDelay {
	case 0:
		c.deliveryRetryDelay = time.Second
	case time.Second:
		c.deliveryRetryDelay = 5 * time.Second
	case 5 * time.Second:
		c.deliveryRetryDelay = 15 * time.Second
	default:
		c.deliveryRetryDelay = 30 * time.Second
	}
	delay := c.deliveryRetryDelay
	c.deliveryRetryScheduled = true
	c.mu.Unlock()
	timer := c.timerAfter(delay)
	go func() {
		select {
		case <-c.stopped:
			return
		case <-timer:
		}
		c.mu.Lock()
		c.deliveryRetryScheduled = false
		c.mu.Unlock()
		if !c.outboundQuiescing() {
			c.queueSettingsRecheck()
		}
	}()
}
