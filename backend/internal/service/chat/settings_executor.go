package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type agentSettingsJournal interface {
	agentSettingsContextReader
	BeginAgentSettingsOperation(context.Context, domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error)
	SupersedeQueuedAgentSettingsOperation(context.Context, string, int64, domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error)
	PendingAgentSettingsOperation(context.Context, domain.SessionID) (domain.AgentSettingsOperation, bool, error)
	LatestAppliedAgentSettingsOperation(context.Context, domain.SessionID) (domain.AgentSettingsOperation, bool, error)
	AgentSettingsOperation(context.Context, domain.SessionID, string) (domain.AgentSettingsOperation, error)
	PinAgentSettingsExecution(context.Context, domain.SessionID, string, int64, domain.AgentSettingsExecution, time.Time) (domain.AgentSettingsOperation, error)
	AdvanceAgentSettingsOperation(context.Context, domain.SessionID, string, int64, domain.AgentSettingsCheckpoint, time.Time) (domain.AgentSettingsOperation, error)
	CommitAgentSettingsOperation(context.Context, domain.SessionID, string, time.Time) (domain.AgentSettingsOperation, error)
	FinishAgentSettingsOperation(context.Context, domain.SessionID, string, int64, domain.AgentSettingsPhase, domain.AgentSettingsErrorCategory, time.Time) (domain.AgentSettingsOperation, error)
	FinishAgentSettingsOperationWithCode(context.Context, domain.SessionID, string, int64, domain.AgentSettingsPhase, domain.AgentSettingsErrorCategory, string, time.Time) (domain.AgentSettingsOperation, error)
	AdvanceAgentSettingsRecovery(context.Context, domain.SessionID, string, int64, domain.AgentSettingsCheckpoint, domain.AgentSettingsRecovery, time.Time) (domain.AgentSettingsOperation, error)
}

// SetAgentSettingsValidator supplies the authorization/discovery boundary
// shared by public mutations and durable recovery.
func (s *Service) SetAgentSettingsValidator(validator ports.ChatAgentSettingsValidator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settingsValidator = validator
	for _, c := range s.ownerControllers {
		c.mu.Lock()
		c.settingsValidator = validator
		c.mu.Unlock()
	}
}

func (s *Service) configureAgentSettingsController(c *Controller) {
	s.mu.Lock()
	validator := s.settingsValidator
	mutator := s.settingsMutator
	s.mu.Unlock()
	c.mu.Lock()
	c.settingsValidator = validator
	c.settingsMutator = mutator
	c.mu.Unlock()
}

// QueueAgentSettings pins the admitted generation throughout acceptance and
// execution without counting its own request as provider activity.
func (s *Service) QueueAgentSettings(ctx context.Context, op domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	c, release, err := s.admitControllerWork(ctx, op.SessionID, true, controllerIntake)
	if err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	defer release()
	return c.QueueAgentSettings(ctx, op)
}

// CancelAgentSettings releases an unstarted intent and wakes an existing queue.
// Cancellation never launches or resumes a provider solely to end the journal.
func (s *Service) CancelAgentSettings(ctx context.Context, id domain.SessionID, operationID string, version int64) (domain.AgentSettingsOperation, error) {
	if c, err := s.Controller(id); err == nil {
		return c.CancelAgentSettings(ctx, operationID, version)
	}
	journal, ok := s.store.(agentSettingsJournal)
	if !ok {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsUnsupported
	}
	return journal.FinishAgentSettingsOperation(ctx, id, operationID, version, domain.AgentSettingsCancelled, domain.AgentSettingsErrorCancelled, s.now())
}

func (c *Controller) rejectPendingAgentSettings(ctx context.Context) error {
	if c.reviewID != "" {
		return nil
	}
	if c.settingsInitializationError != nil {
		return c.settingsInitializationError
	}
	if journal, ok := c.store.(agentSettingsJournal); ok {
		_, found, err := journal.PendingAgentSettingsOperation(ctx, c.sessionID)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("%w: settings_operation_pending", ports.ErrAgentSettingsConflict)
		}
	}
	return nil
}

func ownerForSettings(current domain.SessionAgentSettings) domain.AgentSettingsOwner {
	return domain.AgentSettingsOwner{SessionID: current.SessionID, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID}
}

func (c *Controller) settingsContext(ctx context.Context, journal agentSettingsJournal) (domain.SessionAgentSettings, error) {
	current, err := journal.SessionAgentSettings(ctx, c.sessionID)
	if err != nil {
		return current, err
	}
	owner := ownerForSettings(current)
	if current.IsTerminated || owner != c.settingsObservationOwner || owner.AccountID == "" || owner.ProviderConversationID == "" || owner.BranchID == "" || owner.ControllerGeneration == "" || c.ProviderConversationID() != owner.ProviderConversationID {
		return current, fmt.Errorf("%w: settings_owner_changed", ports.ErrAgentSettingsConflict)
	}
	return current, nil
}

func (c *Controller) qualifySettings(ctx context.Context, op domain.AgentSettingsOperation, owner domain.AgentSettingsOwner, restoring bool) (domain.AgentSettingsQualification, error) {
	c.mu.Lock()
	validator := c.settingsValidator
	c.mu.Unlock()
	if validator == nil {
		return domain.AgentSettingsQualification{}, fmt.Errorf("%w: settings_validation_unavailable", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	proof, err := validator.ValidateAgentSettings(ctx, op, owner, restoring)
	if err != nil {
		return proof, err
	}
	if proof.InputFingerprint == "" || proof.PolicyVersion == "" {
		return proof, fmt.Errorf("%w: settings_validation_unqualified", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	return proof, nil
}

// QueueAgentSettings is an internal command. Busy providers receive only the
// durable intention; the terminal-event drain owns subsequent application.
func (c *Controller) QueueAgentSettings(ctx context.Context, op domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	return c.queueAgentSettings(ctx, op, nil)
}

// SupersedeQueuedAgentSettings coalesces only an unstarted local menu intent.
func (c *Controller) SupersedeQueuedAgentSettings(ctx context.Context, old, next domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	return c.queueAgentSettings(ctx, next, &old)
}

func (c *Controller) queueAgentSettings(ctx context.Context, op domain.AgentSettingsOperation, old *domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	journal, ok := c.store.(agentSettingsJournal)
	if !ok || c.reviewID != "" || op.SessionID != c.sessionID {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsUnsupported
	}
	if _, ok := c.conv.(ports.ChatAgentSettingsController); !ok {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsUnsupported
	}
	if op.ApplyMode == "" {
		op.ApplyMode = domain.AgentSettingsAfterTurn
	}
	if op.ApplyMode != domain.AgentSettingsAfterTurn {
		return domain.AgentSettingsOperation{}, fmt.Errorf("%w: settings_restart_required", ports.ErrAgentSettingsUnsupported)
	}
	current, err := c.settingsContext(ctx, journal)
	if err != nil {
		return op, err
	}
	if op.Source != current.Settings || op.BaseRevision != current.Revision || op.ControllerGeneration != current.OwnerGeneration || op.AccountID != current.AccountID || op.ProviderConversationID != current.ProviderConversationID || op.BranchID != current.BranchID || settingsSelection(c.turnSettings()) != current.Settings {
		return op, ports.ErrAgentSettingsConflict
	}
	if _, err := c.qualifySettings(ctx, op, ownerForSettings(current), false); err != nil {
		return op, err
	}
	if old == nil {
		op, err = journal.BeginAgentSettingsOperation(ctx, op)
	} else {
		op, err = journal.SupersedeQueuedAgentSettingsOperation(ctx, old.ID, old.Version, op)
	}
	if err != nil {
		return op, err
	}
	// Acceptance is a durable CAS, independent of a slow provider dispatch.
	// Only execution owns sendMu; the current wire was already captured before
	// its provider call and the terminal drain observes this pending fence.
	if !c.sendMu.TryLock() {
		c.queueSettingsRecheck()
		return op, nil
	}
	defer c.sendMu.Unlock()
	_, runErr := c.reconcileAgentSettingsLocked(ctx)
	saved, readErr := journal.AgentSettingsOperation(ctx, c.sessionID, op.ID)
	return saved, errors.Join(runErr, readErr)
}

// RunAgentSettings retries a pending/recovery fence under the same dispatch
// lock. It never starts a provider turn until the operation is safely finished.
func (c *Controller) RunAgentSettings(ctx context.Context) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.settingsInitializationError != nil {
		c.settingsInitializationError = nil
		c.settingsInitializationError = c.initializeAgentSettingsLocked(ctx)
		if c.settingsInitializationError != nil {
			return c.settingsInitializationError
		}
	}
	blocked, err := c.reconcileAgentSettingsLocked(ctx)
	if err != nil || blocked {
		return err
	}
	return c.drainLocked(ctx, true)
}

// CancelAgentSettings only ends an unstarted intention. The journal rejects
// cancellation once any provider effect might have happened.
func (c *Controller) CancelAgentSettings(ctx context.Context, id string, version int64) (domain.AgentSettingsOperation, error) {
	journal, ok := c.store.(agentSettingsJournal)
	if !ok || c.reviewID != "" {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsUnsupported
	}
	op, err := journal.FinishAgentSettingsOperation(ctx, c.sessionID, id, version, domain.AgentSettingsCancelled, domain.AgentSettingsErrorCancelled, c.now())
	if err != nil {
		return op, err
	}
	if !c.sendMu.TryLock() {
		c.queueSettingsRecheck()
		return op, nil
	}
	defer c.sendMu.Unlock()
	return op, c.drainLocked(ctx, true)
}

// The lock holder may be a queue edit, a prepare that just lost its CAS, or
// another operation with no terminal-event drain. Do not depend on a new turn.
func (c *Controller) queueSettingsRecheck() {
	c.mu.Lock()
	if c.settingsRecheckQueued {
		c.settingsRecheckRequested = true
		c.mu.Unlock()
		return
	}
	c.settingsRecheckQueued = true
	c.work[controllerReconciliation]++
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			c.settingsRecheckQueued = false
			c.work[controllerReconciliation]--
			recheck := c.settingsRecheckRequested || c.settingsWaitingForWork && c.work[controllerProviderWork] == 0
			c.settingsRecheckRequested = false
			c.mu.Unlock()
			if recheck {
				c.queueSettingsRecheck()
			}
		}()
		c.sendMu.Lock()
		defer c.sendMu.Unlock()
		c.mu.Lock()
		c.settingsRecheckRequested = false
		c.settingsWaitingForWork = false
		stopped := c.state == ports.ChatControllerStopped
		c.mu.Unlock()
		if stopped {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), nativeEditHandoffLimit)
		defer cancel()
		blocked, err := c.reconcileAgentSettingsLocked(ctx)
		if err == nil && !blocked {
			err = c.drainLocked(ctx, true)
		}
		if err != nil {
			// Stable outcome only: provider diagnostics can echo private launch env.
			c.log.Warn("settings recheck retained dispatch fence", "session", c.sessionID, "code", settingsFailureCode(domain.AgentSettingsErrorProvider, []error{err}))
		}
	}()
}

func settingsSelection(wire ports.ChatTurnSettings) domain.AgentSettings {
	fast := wire.FastMode
	if wire.Fast {
		fast = domain.AgentFastOn
	}
	if fast == "" {
		fast = domain.AgentFastDefault
	}
	return domain.AgentSettings{Model: domain.AgentModelSelection(wire.Model), Effort: domain.AgentEffortSelection(wire.Effort), FastMode: fast}
}

func (c *Controller) settingsQuiescent(ctx context.Context) (bool, error) {
	c.mu.Lock()
	busy := c.state != ports.ChatControllerReady || c.handoff != controllerHandoffNone || c.pendingTurnID != "" || c.dispatchingTurnID != "" || c.compactionPending || c.work[controllerProviderWork] != 0
	c.settingsWaitingForWork = c.work[controllerProviderWork] != 0
	c.mu.Unlock()
	if busy {
		return false, nil
	}
	running, err := c.store.ListVisibleRunningTurnProviderIDs(ctx, c.conversation.ID)
	if err != nil {
		return false, err
	}
	if len(running) != 0 {
		return false, nil
	}
	pending, err := c.store.HasPendingConversationInteractions(ctx, c.conversation.ID)
	if err != nil || pending {
		return false, err
	}
	provider, ok := c.conv.(ports.ChatProviderHibernator)
	if !ok {
		return false, fmt.Errorf("%w: provider_quiescence_unavailable", ports.ErrAgentSettingsUnsupported)
	}
	quiet, err := provider.CanHibernate(ctx)
	return quiet, providerSettingsError(err)
}

func (c *Controller) adoptAgentSettings(settings domain.AgentSettings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings.Model = string(settings.Model)
	c.settings.ReasoningEffort = string(settings.Effort)
	c.settings.AgentFastMode = settings.FastMode
	c.settings.FastMode = settings.FastMode == domain.AgentFastOn
}

func (c *Controller) executionStillQualified(ctx context.Context, journal agentSettingsJournal, op domain.AgentSettingsOperation) error {
	current, err := c.settingsContext(ctx, journal)
	if err != nil {
		return err
	}
	if op.Execution == nil || op.Execution.Revision != op.BaseRevision || ownerForSettings(current) != op.Execution.Owner || current.Revision != op.BaseRevision || current.Settings != op.Source {
		return ports.ErrAgentSettingsConflict
	}
	proof, err := c.qualifySettings(ctx, op, ownerForSettings(current), false)
	if err != nil {
		return err
	}
	if proof != op.Execution.Qualification {
		return fmt.Errorf("%w: settings_inputs_or_policy_changed", ports.ErrAgentSettingsConflict)
	}
	return nil
}

// reconcileAgentSettingsLocked runs only with sendMu. blocked means durable
// unfinished work still fences dispatch, including errors or unknown liveness.
func (c *Controller) reconcileAgentSettingsLocked(ctx context.Context) (bool, error) {
	journal, ok := c.store.(agentSettingsJournal)
	if !ok || c.reviewID != "" {
		return false, nil
	}
	if c.settingsInitializationError != nil {
		// A terminal event or a new send can arrive after the provider's
		// baseline becomes available. Retry under the same gate before claiming
		// any queued receipt; initialization recursion sees a cleared error.
		c.settingsInitializationError = nil
		c.settingsInitializationError = c.initializeAgentSettingsLocked(ctx)
		if c.settingsInitializationError != nil {
			return true, c.settingsInitializationError
		}
	}
	op, found, err := journal.PendingAgentSettingsOperation(ctx, c.sessionID)
	if err != nil {
		return true, err
	}
	if !found {
		return false, nil
	}
	quiet, err := c.settingsQuiescent(ctx)
	if err != nil || !quiet {
		return true, err
	}
	if op.StartedAt != nil {
		return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorRecovery)
	}
	current, err := c.settingsContext(ctx, journal)
	if err != nil || current.Settings != op.Source || current.Revision != op.BaseRevision || current.AccountID != op.AccountID || current.ProviderConversationID != op.ProviderConversationID || current.BranchID != op.BranchID {
		return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorConflict)
	}
	proof, err := c.qualifySettings(ctx, op, ownerForSettings(current), false)
	if err != nil {
		return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorValidation, err)
	}
	provider, ok := c.conv.(ports.ChatAgentSettingsController)
	if !ok {
		return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorValidation, err)
	}
	plan, err := provider.PrepareAgentSettings(ctx, op.Source, op.Target)
	if err != nil {
		return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorValidation, providerSettingsError(err))
	}
	execution := domain.AgentSettingsExecution{Owner: ownerForSettings(current), Revision: current.Revision, Qualification: proof, Plan: plan}
	op, err = journal.PinAgentSettingsExecution(ctx, c.sessionID, op.ID, op.Version, execution, c.now())
	if err != nil {
		return true, err
	}
	for _, step := range []domain.AgentSettingsCheckpoint{domain.AgentSettingsApplyingModel, domain.AgentSettingsApplyingEffort, domain.AgentSettingsApplyingFast, domain.AgentSettingsVerifying} {
		if err := c.executionStillQualified(ctx, journal, op); err != nil {
			return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorValidation, err)
		}
		next, err := journal.AdvanceAgentSettingsOperation(ctx, c.sessionID, op.ID, op.Version, step, c.now())
		if err != nil {
			return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorStorage, err)
		}
		op = next
		if err := provider.ApplyAgentSettingsStep(ctx, plan, step, false); err != nil {
			return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorProvider, providerSettingsError(err))
		}
	}
	verified, err := journal.AdvanceAgentSettingsOperation(ctx, c.sessionID, op.ID, op.Version, domain.AgentSettingsVerified, c.now())
	if err != nil {
		return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorStorage, err)
	}
	op = verified
	if err := c.executionStillQualified(ctx, journal, op); err != nil {
		return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorValidation, err)
	}
	applied, err := journal.CommitAgentSettingsOperation(ctx, c.sessionID, op.ID, c.now())
	if err != nil {
		return c.failAgentSettingsLocked(ctx, journal, op, domain.AgentSettingsErrorStorage, err)
	}
	c.adoptAgentSettings(applied.Target)
	return false, nil
}

func (c *Controller) failAgentSettingsLocked(ctx context.Context, journal agentSettingsJournal, op domain.AgentSettingsOperation, category domain.AgentSettingsErrorCategory, causes ...error) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	latest, err := journal.AgentSettingsOperation(ctx, c.sessionID, op.ID)
	if err != nil {
		return true, err
	}
	if latest.Phase == domain.AgentSettingsApplied {
		c.adoptAgentSettings(latest.Target)
		return false, nil
	} // Commit acknowledgement was lost.
	if latest.Phase == domain.AgentSettingsFailed || latest.Phase == domain.AgentSettingsCancelled {
		return false, nil
	}
	code := latest.ErrorCode
	if code == "" {
		code = settingsFailureCode(category, causes)
	}
	if latest.StartedAt == nil {
		_, err := journal.FinishAgentSettingsOperationWithCode(ctx, c.sessionID, latest.ID, latest.Version, domain.AgentSettingsFailed, category, code, c.now())
		return err != nil, err
	}
	if latest.Phase == domain.AgentSettingsPending {
		latest, err = journal.FinishAgentSettingsOperationWithCode(ctx, c.sessionID, latest.ID, latest.Version, domain.AgentSettingsRecoveryRequired, category, code, c.now())
		if err != nil {
			return true, err
		}
	}
	return c.recoverAgentSettingsLocked(ctx, journal, latest)
}

func settingsFailureCode(category domain.AgentSettingsErrorCategory, causes []error) string {
	for _, cause := range causes {
		var api *apierr.Error
		if errors.As(cause, &api) && strings.HasPrefix(api.Code, "AGENT_SETTINGS_") && len(api.Code) <= 96 && !strings.ContainsFunc(api.Code, func(r rune) bool { return r != '_' && (r < 'A' || r > 'Z') && (r < '0' || r > '9') }) {
			return api.Code
		}
		if errors.Is(cause, ports.ErrAgentSettingsCapabilitiesUnavailable) {
			return "AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE"
		}
		if errors.Is(cause, ports.ErrAgentSettingsUnsupported) {
			return "AGENT_SETTINGS_UNSUPPORTED"
		}
		if errors.Is(cause, ports.ErrAgentSettingsConflict) {
			return "AGENT_SETTINGS_CONFLICT"
		}
	}
	switch category {
	case domain.AgentSettingsErrorProvider:
		return "AGENT_SETTINGS_PROVIDER_FAILED"
	case domain.AgentSettingsErrorStorage:
		return "AGENT_SETTINGS_STORAGE_FAILED"
	case domain.AgentSettingsErrorConflict:
		return "AGENT_SETTINGS_CONFLICT"
	case domain.AgentSettingsErrorRecovery:
		return "AGENT_SETTINGS_RECOVERY_REQUIRED"
	default:
		return "AGENT_SETTINGS_VALIDATION_FAILED"
	}
}
