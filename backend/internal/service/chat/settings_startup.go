package chat

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Ownership is already committed and the old writer positively stopped by the
// existing start/branch boundary. Initialize before publication/private prompts,
// outside Service.mu. Failure publishes a fenced controller, never an unowned
// provider or a usable controller carrying unverified sticky settings.
func (c *Controller) initializeAgentSettings(ctx context.Context) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.settingsInitializationError = c.initializeAgentSettingsLocked(ctx)
	if c.settingsInitializationError != nil {
		c.log.Warn("Chat settings initialization keeps dispatch fenced", "session", c.sessionID, "reason", "agent_settings_unavailable")
	}
}

func (c *Controller) initializeAgentSettingsLocked(ctx context.Context) error {
	journal, ok := c.store.(agentSettingsJournal)
	if !ok || c.reviewID != "" {
		return nil
	}
	_, pending, err := journal.PendingAgentSettingsOperation(ctx, c.sessionID)
	if err != nil {
		return err
	}
	if pending {
		_, err := c.reconcileAgentSettingsLocked(ctx)
		return err
	}
	op, found, err := journal.LatestAppliedAgentSettingsOperation(ctx, c.sessionID)
	if err != nil {
		return err
	}
	if !found || op.Execution == nil {
		return nil
	} // Legacy operations did not install typed wire.
	current, err := c.settingsContext(ctx, journal)
	if err != nil {
		return err
	}
	if current.Settings != op.Target || current.Revision != op.BaseRevision+1 {
		return fmt.Errorf("%w: committed_settings_changed", ports.ErrAgentSettingsConflict)
	}
	quiet, err := c.settingsQuiescent(ctx)
	if err != nil {
		return err
	}
	if !quiet {
		return fmt.Errorf("%w: provider_quiescence_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	owner := ownerForSettings(current)
	replay := op
	replay.Source = current.Settings
	replay.Target = current.Settings
	replay.BaseRevision = current.Revision
	proof, err := c.qualifySettings(ctx, replay, owner, true)
	if err != nil {
		return err
	}
	provider, ok := c.conv.(ports.ChatAgentSettingsController)
	if !ok {
		return ports.ErrAgentSettingsUnsupported
	}
	var plan domain.AgentSettingsProviderPlan
	if baseline, ok := c.conv.(ports.ChatAgentSettingsRecoveryPreparer); ok {
		// Per-turn wire needs only the committed preference and a fresh baseline.
		// A completed operation's source may no longer be offered by the provider.
		plan, err = baseline.PrepareAgentSettingsRecovery(ctx, current.Settings)
		if err == nil && (plan.Source != current.Settings || plan.Target != current.Settings || plan.ProviderConversationID != owner.ProviderConversationID) {
			return ports.ErrAgentSettingsConflict
		}
	} else {
		// Stateful setters still need the explicit/default transition markers to
		// prove resets; the old native values are never a new-process baseline.
		plan, err = provider.PrepareAgentSettings(ctx, op.Source, current.Settings)
	}
	if err != nil {
		return err
	}
	for _, step := range []domain.AgentSettingsCheckpoint{domain.AgentSettingsApplyingModel, domain.AgentSettingsApplyingEffort, domain.AgentSettingsApplyingFast, domain.AgentSettingsVerifying} {
		if err := c.coldSettingsStillQualified(ctx, journal, replay, current, proof); err != nil {
			return err
		}
		if err := provider.ApplyAgentSettingsStep(ctx, plan, step, false); err != nil {
			return err
		}
	}
	if err := c.coldSettingsStillQualified(ctx, journal, replay, current, proof); err != nil {
		return err
	}
	c.adoptAgentSettings(current.Settings)
	return nil
}

func (c *Controller) coldSettingsStillQualified(ctx context.Context, journal agentSettingsJournal, replay domain.AgentSettingsOperation, expected domain.SessionAgentSettings, proof domain.AgentSettingsQualification) error {
	current, err := c.settingsContext(ctx, journal)
	if err != nil {
		return err
	}
	if current != expected {
		return ports.ErrAgentSettingsConflict
	}
	currentProof, err := c.qualifySettings(ctx, replay, ownerForSettings(current), true)
	if err != nil {
		return err
	}
	if currentProof != proof {
		return ports.ErrAgentSettingsConflict
	}
	return nil
}
