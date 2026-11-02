package chat

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Recovery uses the current durable preference. A preimage from a different
// account, native branch, binary input or revision cannot prove its restoration.
// Such cases retain recovery_required; they never guess a provider baseline.
func (c *Controller) recoverAgentSettingsLocked(ctx context.Context, journal agentSettingsJournal, op domain.AgentSettingsOperation) (bool, error) {
	current, err := c.settingsContext(ctx, journal)
	if err != nil {
		return true, err
	}
	owner := ownerForSettings(current)
	recoveryIntent := op
	recoveryIntent.Source = current.Settings
	recoveryIntent.Target = current.Settings
	recoveryIntent.BaseRevision = current.Revision
	proof, err := c.qualifySettings(ctx, recoveryIntent, owner, true)
	if err != nil {
		return true, err
	}
	// Claim recovery even when the old preimage is unusable, preserving the
	// explicit recovery state and the current qualified owner for later repair.
	expected := domain.AgentSettingsRecovery{Owner: owner, Revision: current.Revision, Settings: current.Settings}
	op, err = journal.AdvanceAgentSettingsRecovery(ctx, c.sessionID, op.ID, op.Version, domain.AgentSettingsRollingBack, expected, c.now())
	if err != nil {
		return true, err
	}
	previous := op.Execution
	provider, ok := c.conv.(ports.ChatAgentSettingsController)
	if !ok {
		return true, ports.ErrAgentSettingsUnsupported
	}
	var plan domain.AgentSettingsProviderPlan
	if previous != nil && previous.Owner.AccountID == owner.AccountID && previous.Owner.ProviderConversationID == owner.ProviderConversationID && previous.Owner.BranchID == owner.BranchID && previous.Qualification.InputFingerprint == proof.InputFingerprint && previous.Revision == current.Revision && previous.Plan.Source == current.Settings {
		plan = previous.Plan
	} else {
		fresh, ok := c.conv.(ports.ChatAgentSettingsRecoveryPreparer)
		if !ok {
			return true, fmt.Errorf("%w: recovery_baseline_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
		err = c.settingsRPC(ctx, "preparing", false, func(ctx context.Context) error {
			plan, err = fresh.PrepareAgentSettingsRecovery(ctx, current.Settings)
			return err
		})
		if err != nil {
			return true, providerSettingsError(err)
		}
		if plan.Source != current.Settings || plan.Target != current.Settings || plan.ProviderConversationID != owner.ProviderConversationID {
			return true, ports.ErrAgentSettingsConflict
		}
	}
	if err := c.settingsRPC(ctx, "rolling_back", true, func(ctx context.Context) error {
		return provider.ApplyAgentSettingsStep(ctx, plan, domain.AgentSettingsRollingBack, true)
	}); err != nil {
		return true, providerSettingsError(err)
	}
	fresh, err := c.settingsContext(ctx, journal)
	if err != nil {
		return true, err
	}
	if ownerForSettings(fresh) != owner || fresh.Settings != current.Settings || fresh.Revision != current.Revision {
		return true, ports.ErrAgentSettingsConflict
	}
	freshProof, err := c.qualifySettings(ctx, recoveryIntent, owner, true)
	if err != nil {
		return true, err
	}
	if freshProof != proof {
		return true, ports.ErrAgentSettingsConflict
	}
	op, err = journal.AdvanceAgentSettingsRecovery(ctx, c.sessionID, op.ID, op.Version, domain.AgentSettingsRestored, expected, c.now())
	if err != nil {
		return true, err
	}
	category := op.ErrorCategory
	if category == "" {
		category = domain.AgentSettingsErrorProvider
	}
	if _, err := journal.FinishAgentSettingsOperation(ctx, c.sessionID, op.ID, op.Version, domain.AgentSettingsFailed, category, c.now()); err != nil {
		return true, err
	}
	c.adoptAgentSettings(current.Settings)
	return false, nil
}
