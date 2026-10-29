package chat

import (
	"context"
	"slices"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (c *Controller) pendingLocalSettings(ctx context.Context) (domain.AgentSettingsOperation, bool, error) {
	if c.agentSettingsMutator() == nil {
		return domain.AgentSettingsOperation{}, false, nil
	}
	journal, ok := c.store.(agentSettingsJournal)
	if !ok {
		return domain.AgentSettingsOperation{}, false, nil
	}
	op, found, err := journal.PendingAgentSettingsOperation(ctx, c.sessionID)
	owner := c.settingsObservationOwner
	return op, found && op.CanSupersedeLocally() && op.ControllerGeneration == owner.ControllerGeneration && op.AccountID == owner.AccountID && op.ProviderConversationID == owner.ProviderConversationID && op.BranchID == owner.BranchID, err
}

// Menu projection is separate from c.settings: pending intent must never change
// the current turn's wire, canonical preference, or effective observation.
func (c *Controller) projectedLegacySettings(ctx context.Context, base domain.ConversationSettings) (domain.ConversationSettings, error) {
	op, found, err := c.pendingLocalSettings(ctx)
	if err != nil || !found {
		return base, err
	}
	base.Model, base.ReasoningEffort = string(op.Target.Model), string(op.Target.Effort)
	base.AgentFastMode, base.FastMode = op.Target.FastMode, op.Target.FastMode == domain.AgentFastOn
	return base, nil
}

func (c *Controller) projectedLegacyOptions(ctx context.Context, options []ports.ChatConfigOption) ([]ports.ChatConfigOption, error) {
	op, found, err := c.pendingLocalSettings(ctx)
	if err != nil || !found {
		return options, err
	}
	options = slices.Clone(options)
	for i, option := range options {
		switch {
		case (option.ID == "model" || option.Category == "model") && op.Target.Model != op.Source.Model:
			options[i].Current.Select = projectedDefaultOption(option, string(op.Target.Model))
		case (option.ID == "effort" || option.Category == "thought_level") && op.Target.Effort != op.Source.Effort:
			options[i].Current.Select = projectedDefaultOption(option, string(op.Target.Effort))
		case option.ID == "fast" && op.Target.FastMode != op.Source.FastMode && op.Target.FastMode != domain.AgentFastDefault:
			if option.Type == ports.ChatConfigOptionBoolean {
				options[i].Current.Boolean = new(op.Target.FastMode == domain.AgentFastOn)
			} else {
				options[i].Current.Select = string(op.Target.FastMode)
			}
		}
	}
	return options, nil
}

func projectedDefaultOption(option ports.ChatConfigOption, value string) string {
	if value == "" && slices.ContainsFunc(option.Choices, func(choice ports.ChatConfigOptionChoice) bool { return choice.Value == "default" }) {
		return "default"
	}
	return value
}
