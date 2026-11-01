package chat

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type settingsReconciliationJournal interface {
	BeginAgentSettingsReconciliation(context.Context, domain.AgentSettingsReconciliation) (domain.AgentSettingsReconciliation, error)
	SaveAgentSettingsReconciliation(context.Context, domain.AgentSettingsReconciliation, int64, string) (domain.AgentSettingsReconciliation, error)
	ListAgentSettingsReconciliations(context.Context, domain.SessionID) ([]domain.AgentSettingsReconciliation, error)
}

func (c *Controller) timerAfter(delay time.Duration) <-chan time.Time {
	if c.after != nil {
		return c.after(delay)
	}
	return time.After(delay)
}

type chatDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c chatDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

// A single injected timer drives cancellation and the advertised deadline, so
// fake-clock tests exercise the same boundary as real provider RPCs.
func (c *Controller) deadlineContext(parent context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	// Context deadlines are consumed by net/http, exec and provider SDKs, whose
	// clocks are real. The injected timer controls cancellation independently of
	// the business clock used in durable diagnostics.
	remaining := limit
	deadline := time.Now().Add(remaining)
	if d, ok := parent.Deadline(); ok && d.Before(deadline) {
		deadline = d
		remaining = time.Until(d)
	}
	ctx, cancel := context.WithCancelCause(parent)
	if remaining <= 0 {
		cancel(context.DeadlineExceeded)
		return chatDeadlineContext{Context: ctx, deadline: deadline}, func() { cancel(nil) }
	}
	timer := c.timerAfter(remaining)
	go func() {
		select {
		case <-timer:
			cancel(context.DeadlineExceeded)
		case <-ctx.Done():
		}
	}()
	return chatDeadlineContext{Context: ctx, deadline: deadline}, func() { cancel(nil) }
}

func (c *Controller) saveSettingsAttemptLocked(ctx context.Context, next domain.AgentSettingsReconciliation) error {
	journal, ok := c.store.(settingsReconciliationJournal)
	if !ok || c.settingsAttempt == nil {
		return nil
	}
	previous := c.settingsAttempt
	next.Version = previous.Version + 1
	next.ExecutorID = previous.ExecutorID
	saved, err := journal.SaveAgentSettingsReconciliation(ctx, next, previous.Version, previous.ExecutorID)
	if err == nil {
		c.settingsAttempt = &saved
	}
	return err
}

type settingsAttemptOperationReader interface {
	AgentSettingsOperation(context.Context, domain.SessionID, string) (domain.AgentSettingsOperation, error)
}

// A completed/cancelled business operation cannot lend its work record to the
// next intent. An uncertain external effect remains fenced regardless of phase.
func (c *Controller) finishObsoleteSettingsAttemptLocked(ctx context.Context) error {
	a := c.settingsAttempt
	if a == nil || a.EffectPending || a.Phase == "ready" || a.Phase == "cancelled" || a.OperationID == "" {
		return nil
	}
	reader, ok := c.store.(settingsAttemptOperationReader)
	if !ok {
		return nil
	}
	op, err := reader.AgentSettingsOperation(ctx, c.sessionID, a.OperationID)
	if err != nil {
		return err
	}
	next := *a
	switch {
	case op.Phase == domain.AgentSettingsCancelled || op.Phase == domain.AgentSettingsFailed:
		next.Phase = "cancelled"
	case op.Phase == domain.AgentSettingsApplied && op.BaseRevision == a.Revision:
		next.Phase = "ready"
	default:
		return nil
	}
	next.LastProgressAt = laterChatTime(c.now(), next.LastProgressAt)
	next.RecoveryActions = []string{}
	if err := c.saveSettingsAttemptLocked(ctx, next); err != nil {
		return err
	}
	c.settingsAttempt = nil
	return nil
}

func (c *Controller) beginSettingsAttemptLocked(ctx context.Context, cold bool) error {
	if err := c.finishObsoleteSettingsAttemptLocked(ctx); err != nil {
		return err
	}
	journal, ok := c.store.(agentSettingsJournal)
	attempts, durable := c.store.(settingsReconciliationJournal)
	if !ok || !durable || c.reviewID != "" {
		return nil
	}
	op, found, err := journal.PendingAgentSettingsOperation(ctx, c.sessionID)
	if err != nil {
		return err
	}
	if !found {
		if !cold && c.settingsInitializationError == nil {
			return nil
		}
		op, found, err = journal.LatestAppliedAgentSettingsOperation(ctx, c.sessionID)
		if err != nil || !found || op.Execution == nil {
			return err
		}
	}
	current, err := c.settingsContext(ctx, journal)
	if err != nil {
		return err
	}
	if c.settingsAttempt != nil && c.settingsAttempt.Phase != "ready" && c.settingsAttempt.Phase != "cancelled" {
		a := c.settingsAttempt
		if a.Owner != ownerForSettings(current) || a.Revision != current.Revision {
			return ports.ErrAgentSettingsConflict
		}
		if a.EffectPending {
			return ports.ErrAgentSettingsConflict
		}
		if a.Phase == "blocked" && a.RetryCount >= 3 {
			return ports.ErrAgentSettingsCapabilitiesUnavailable
		}
		if a.Phase != "waiting_idle" && !a.Deadline.After(c.now()) {
			return context.DeadlineExceeded
		}
		return nil
	}
	previous, err := attempts.ListAgentSettingsReconciliations(ctx, c.sessionID)
	if err != nil {
		return err
	}
	for _, a := range previous {
		if a.OperationID == op.ID && a.EffectPending {
			// A new controller cannot infer that an unacknowledged effect did not run.
			// Keep its original action key; only qualified observation can unlock it.
			c.settingsAttempt = &a
			return ports.ErrAgentSettingsConflict
		}
	}
	if c.settingsExecutorID == "" {
		c.settingsExecutorID = uuid.NewString()
	}
	at := c.now()
	r := domain.AgentSettingsReconciliation{
		RetryOfAttemptID: c.settingsRetrySourceID, RetryOfVersion: c.settingsRetrySourceVersion,
		ID: uuid.NewString(), SessionID: c.sessionID, Owner: ownerForSettings(current), Revision: current.Revision,
		OperationID: op.ID, Phase: "observing", Step: "observing", ExecutorID: c.settingsExecutorID,
		ClaimUntil: at.Add(30 * time.Second), Deadline: at.Add(30 * time.Second), CreatedAt: at, LastProgressAt: at,
		RequestID: op.RequestID, Desired: op.Target, PreferenceAtStart: current.Settings, ObservationAvailability: "unknown",
		EvidenceKind:    "unknown",
		MissingEvidence: []string{"current_settings", "provider_epoch"}, RecoveryActions: []string{"wait_for_provider"},
	}
	if op.Phase == domain.AgentSettingsApplied {
		historical := op.Target
		r.HistoricalApplied = &historical
	}
	saved, err := attempts.BeginAgentSettingsReconciliation(ctx, r)
	if err != nil {
		return err
	}
	c.settingsAttempt = &saved
	return nil
}

func (c *Controller) runSettingsAttemptLocked(ctx context.Context, cold bool, work func(context.Context) (bool, error)) (bool, error) {
	if c.settingsAttemptRunning {
		return work(ctx)
	}
	if err := c.beginSettingsAttemptLocked(ctx, cold); err != nil {
		return true, err
	}
	c.settingsAttemptRunning = true
	defer func() { c.settingsAttemptRunning = false }()
	blocked, runErr := work(ctx)
	if c.settingsAttempt == nil || c.settingsAttempt.Phase == "ready" || c.settingsAttempt.Phase == "cancelled" {
		return blocked, runErr
	}
	next := *c.settingsAttempt
	if next.EffectPending {
		blocked = true
		if runErr == nil {
			runErr = ports.ErrAgentSettingsConflict
		}
		next.Phase = "recovery_required"
	} else if runErr != nil {
		next.Phase = "blocked"
	} else if blocked {
		next.Phase = "waiting_idle"
		next.RecoveryActions = []string{"wait_for_work"}
		if next.BlockedSince == nil {
			at := c.now()
			next.BlockedSince = &at
		}
	} else {
		next.Phase = "ready"
		next.RecoveryActions = []string{}
	}
	next.LastProgressAt = c.now()
	if runErr != nil && (!next.EffectPending || next.ErrorCode == "") {
		setSettingsAttemptFailure(&next, runErr, c.now())
	}
	if !next.EffectPending && next.OperationID != "" {
		if reader, ok := c.store.(settingsAttemptOperationReader); ok {
			op, err := reader.AgentSettingsOperation(ctx, c.sessionID, next.OperationID)
			if err == nil && op.Phase == domain.AgentSettingsFailed {
				next.RecoveryActions = []string{"submit_new_settings"}
			}
			if err == nil && op.Phase == domain.AgentSettingsCancelled {
				next.Phase = "cancelled"
				next.RecoveryActions = []string{}
			}
		}
	}
	// Persist the diagnosis even after request cancellation. A failed outcome
	// write leaves the previous checkpoint, including its uncertainty, intact.
	outcomeCtx, cancel := c.deadlineContext(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.saveSettingsAttemptLocked(outcomeCtx, next); err != nil {
		return true, errors.Join(runErr, err)
	}
	if runErr != nil {
		c.log.Warn("settings reconciliation retained dispatch fence", "session", c.sessionID, "attempt", next.ID, "step", next.FailedStep, "code", next.ErrorCode, "cause", next.SafeCause, "request", next.RequestID)
	}
	return blocked, runErr
}

// retrySettingsAttemptLocked starts a new read-only attempt on explicit retry.
// It cannot cancel or hide an uncertain side effect.
func (c *Controller) retrySettingsAttemptLocked(ctx context.Context) error {
	if c.settingsAttempt == nil || c.settingsAttempt.Phase == "ready" || c.settingsAttempt.Phase == "cancelled" {
		return nil
	}
	next := *c.settingsAttempt
	if next.EffectPending {
		return ports.ErrAgentSettingsConflict
	}
	next.Phase = "cancelled"
	next.LastProgressAt = c.now()
	if err := c.saveSettingsAttemptLocked(ctx, next); err != nil {
		return err
	}
	c.settingsAttempt = nil
	return nil
}

func (c *Controller) settingsRetryDelayLocked() time.Duration {
	a := c.settingsAttempt
	if a == nil || a.EffectPending || a.Phase != "blocked" || a.RetryCount >= 3 {
		return 0
	}
	if a.SafeCause != "capabilities_unavailable" && a.SafeCause != "timeout" && a.ProviderErrorCode != "transport_unavailable" {
		return 0
	}
	delay := [...]time.Duration{time.Second, 5 * time.Second, 15 * time.Second}[a.RetryCount]
	if !a.Deadline.After(c.now().Add(delay)) {
		return 0
	}
	return delay
}

func (c *Controller) waitSettingsRetry(delay time.Duration) bool {
	select {
	case <-c.timerAfter(delay):
		return true
	case <-c.stopped:
		return false
	}
}

func (c *Controller) runSettingsRechecks() {
	for {
		c.sendMu.Lock()
		c.mu.Lock()
		c.settingsRecheckRequested = false
		c.settingsWaitingForWork = false
		stopped := c.state == ports.ChatControllerStopped
		c.mu.Unlock()
		if stopped {
			c.sendMu.Unlock()
			return
		}
		if delay := c.settingsRetryDelayLocked(); delay > 0 {
			id, version := c.settingsAttempt.ID, c.settingsAttempt.Version
			c.sendMu.Unlock()
			if !c.waitSettingsRetry(delay) {
				return
			}
			c.sendMu.Lock()
			if c.settingsAttempt == nil || c.settingsAttempt.ID != id || c.settingsAttempt.Version != version {
				c.sendMu.Unlock()
				continue
			}
			next := *c.settingsAttempt
			next.RetryCount++
			next.Phase = "observing"
			next.LastProgressAt = c.now()
			ctx, cancel := c.deadlineContext(context.Background(), 10*time.Second)
			err := c.saveSettingsAttemptLocked(ctx, next)
			cancel()
			if err != nil {
				c.sendMu.Unlock()
				return
			}
		}
		ctx, cancel := c.deadlineContext(context.Background(), 30*time.Second)
		blocked, err := c.reconcileAgentSettingsLocked(ctx)
		if err == nil && !blocked {
			err = c.drainLocked(ctx, true)
		}
		cancel()
		retry := err != nil && c.settingsRetryDelayLocked() > 0
		c.sendMu.Unlock()
		if !retry {
			return
		}
	}
}

func setSettingsAttemptFailure(a *domain.AgentSettingsReconciliation, err error, at time.Time) {
	a.FailedStep = a.Step
	a.FailedAt = &at
	if a.BlockedSince == nil {
		a.BlockedSince = &at
	}
	a.ErrorCategory = string(domain.AgentSettingsErrorProvider)
	a.ErrorCode = settingsFailureCode(domain.AgentSettingsErrorProvider, []error{err})
	a.SafeCause = "provider_unclassified"
	a.ProviderErrorCode = "unknown"
	var rpcFailure interface{ AgentSettingsJSONRPCCode() int }
	if errors.As(err, &rpcFailure) {
		code := rpcFailure.AgentSettingsJSONRPCCode()
		a.JSONRPCCode = &code
		switch code {
		case -32601:
			a.ProviderErrorCode = "unsupported"
		case -32602:
			a.ProviderErrorCode = "invalid_request"
		}
	}

	a.RecoveryActions = []string{"retry"}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		a.SafeCause = "timeout"
	case errors.Is(err, context.Canceled):
		a.SafeCause = "cancelled"
	case errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable):
		a.SafeCause = "capabilities_unavailable"
		a.RecoveryActions = []string{"wait_for_provider", "retry"}
	case errors.Is(err, ports.ErrAgentSettingsUnsupported):
		a.SafeCause = "unsupported"
		a.RecoveryActions = []string{"wait_for_provider"}
	case errors.Is(err, ports.ErrAgentSettingsConflict):
		a.SafeCause = "owner_changed"
		a.RecoveryActions = []string{"wait_for_provider"}
	}
	if a.EffectPending {
		a.RecoveryActions = []string{"wait_for_provider"}
	}
}

// settingsRPC checkpoints before an external call and acknowledges only its
// returned outcome. Cancellation is never treated as proof of no side effect.
func (c *Controller) settingsRPC(ctx context.Context, step string, effect bool, call func(context.Context) error) error {
	if c.outboundQuiescing() {
		return ports.ErrChatShutdownNotSettled
	}
	active := c.settingsAttemptRunning && c.settingsAttempt != nil
	if active && c.settingsAttempt.EvidenceKind == "per_turn_wire" {
		// These steps prepare an immutable request; they do not mutate sticky
		// provider settings. The eventual turn has its own delivery reservation.
		effect = false
	}
	if active {
		next := *c.settingsAttempt
		if next.EffectPending {
			return ports.ErrAgentSettingsConflict
		}
		next.Step, next.EffectPending = step, effect
		next.Phase = "observing"
		if effect {
			next.Phase = "applying"
			next.ActionKey = next.ID + ":" + step
		} else if step == "verifying" {
			next.Phase = "verifying"
		} else if step == "waiting_idle" {
			next.Phase = "waiting_idle"
			if !next.Deadline.After(c.now()) {
				next.Deadline = c.now().Add(30 * time.Second)
				next.ClaimUntil = next.Deadline
			}
		}
		next.LastProgressAt = c.now()
		if err := c.saveSettingsAttemptLocked(ctx, next); err != nil {
			return err
		}
	}
	limit := 10 * time.Second
	if a := c.settingsAttempt; active && a.Phase != "waiting_idle" {
		limit = min(limit, a.Deadline.Sub(c.now()))
	}
	stepCtx, cancel := c.deadlineContext(ctx, limit)
	defer cancel()
	started := stepCtx.Err() == nil
	var err error
	if started {
		err = call(stepCtx)
	} else {
		err = context.Cause(stepCtx)
	}
	if errors.Is(context.Cause(stepCtx), context.DeadlineExceeded) {
		err = context.DeadlineExceeded
	}
	if active {
		next := *c.settingsAttempt
		next.LastProgressAt = c.now()
		if err == nil {
			next.EffectPending = false
			next.LastSuccessfulStep = step
		} else {
			next.EffectPending = effect && started
			next.Phase = "blocked"
			if next.EffectPending {
				next.Phase = "recovery_required"
			}
			setSettingsAttemptFailure(&next, err, c.now())
		}
		outcomeCtx, outcomeCancel := c.deadlineContext(context.WithoutCancel(ctx), 10*time.Second)
		defer outcomeCancel()
		if saveErr := c.saveSettingsAttemptLocked(outcomeCtx, next); saveErr != nil {
			return errors.Join(providerSettingsError(err), saveErr)
		}
	}
	return err
}

func (c *Controller) qualifySettingsEvidenceLocked(ctx context.Context, target *domain.AgentSettings) error {
	if c.settingsAttempt == nil || !c.settingsAttemptRunning {
		return nil
	}
	provider, ok := c.conv.(ports.ChatAgentSettingsReconciliationEvidence)
	if !ok {
		return ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	var proof ports.AgentSettingsReconciliationEvidence
	err := c.settingsRPC(ctx, "observing", false, func(ctx context.Context) error {
		var err error
		proof, err = provider.ReconciliationEvidence(ctx, c.settingsObservationOwner)
		return err
	})
	if err != nil {
		return providerSettingsError(err)
	}
	if proof.Owner != c.settingsObservationOwner || !proof.PermissionVerified || proof.SourceRef == "" {
		return ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	next := *c.settingsAttempt
	next.EvidenceKind, next.ObservationRef = proof.Kind, proof.SourceRef
	switch proof.Kind {
	case "per_turn_wire":
		next.Observed = nil
		next.ObservationAvailability = "unknown"
		next.MissingEvidence = []string{}
	case "provider_current":
		if proof.ProviderEpoch == "" || proof.Settings == nil || proof.Settings.Validate() != nil {
			return ports.ErrAgentSettingsCapabilitiesUnavailable
		}
		if target != nil && *proof.Settings != *target {
			return ports.ErrAgentSettingsConflict
		}
		next.ProviderEpoch = proof.ProviderEpoch
		settings := *proof.Settings
		next.Observed = &settings
		next.ObservationAvailability = "available"
		next.MissingEvidence = []string{}
	default:
		return ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	return c.saveSettingsAttemptLocked(ctx, next)
}
