package acp

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.ChatAgentSettingsController = (*conversation)(nil)

// settingsCatalog excludes optimistic accepted-value projections. A setter
// acknowledgement without its rebuilt catalog cannot authorize another setter.
func (c *conversation) settingsCatalog() ([]ports.ChatConfigOption, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.agentSettingsDefaults == nil {
		return nil, fmt.Errorf("%w: settings_runtime_unsupported", ports.ErrAgentSettingsUnsupported)
	}
	if c.settingsObservedAt.IsZero() || c.settingsReceiptSessionID != c.sessionID {
		return nil, fmt.Errorf("%w: provider_catalog_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	return cloneConfigOptions(c.settingsReceipts), nil
}

func nativeSettings(options []ports.ChatConfigOption) (domain.AgentSettingsNativeValues, string) {
	var values domain.AgentSettingsNativeValues
	var permission string
	for _, option := range options {
		value := option.Current.Select
		if option.Current.Boolean != nil {
			value = "off"
			if *option.Current.Boolean {
				value = "on"
			}
		}
		switch option.ID {
		case "model":
			values.Model = value
		case "effort":
			values.Effort = value
		case "fast":
			values.Fast = value
		case "mode":
			permission = value
		}
	}
	return values, permission
}

func settingsValue(values domain.AgentSettingsNativeValues, id string) string {
	switch id {
	case "model":
		return values.Model
	case "effort":
		return values.Effort
	case "fast":
		return values.Fast
	}
	return ""
}

func selectionValue(settings domain.AgentSettings, id string) string {
	switch id {
	case "model":
		return string(settings.Model)
	case "effort":
		return string(settings.Effort)
	case "fast":
		if settings.FastMode != domain.AgentFastDefault {
			return string(settings.FastMode)
		}
	}
	return ""
}

// targetOption resolves only binding-defined semantics against the current
// authoritative catalog. Unchanged default fields retain their proven preimage.
func (c *conversation) targetOption(plan domain.AgentSettingsProviderPlan, id string, restoring bool, catalog []ports.ChatConfigOption) (string, error) {
	// An untouched implicit dependent field is not a reset request. Let the
	// new model's authoritative catalog supply it without making a native fast
	// default into an explicit paid choice. Explicit -> default still needs proof.
	if !restoring && id != "model" && selectionValue(plan.Source, id) == "" && selectionValue(plan.Target, id) == "" {
		return "", nil
	}
	value := selectionValue(plan.Target, id)
	if restoring {
		value = settingsValue(plan.Before, id)
	} else if value == "" {
		if selectionValue(plan.Source, id) == "" && (id == "model" || plan.Source.Model == plan.Target.Model) {
			value = settingsValue(plan.Before, id)
		} else {
			value = c.agentSettingsDefaults[id]
			if value == "" {
				return "", fmt.Errorf("%w: %s_reset_requires_restart", ports.ErrAgentSettingsUnsupported, id)
			}
		}
	}
	var option *ports.ChatConfigOption
	for i := range catalog {
		if catalog[i].ID == id {
			option = &catalog[i]
			break
		}
	}
	if option == nil {
		// A model-dependent option can remain dormant when its preference did
		// not change. Its effective value stays not_provided.
		if value == "" || !restoring && selectionValue(plan.Source, id) == selectionValue(plan.Target, id) && id != "model" {
			return "", nil
		}
		return "", fmt.Errorf("%w: %s_not_offered", ports.ErrAgentSettingsUnsupported, id)
	}
	if value == "" {
		return "", fmt.Errorf("%w: %s_default_unverified", ports.ErrAgentSettingsUnsupported, id)
	}
	if id == "model" && !restoring && c.optionsFor != nil {
		for _, mapped := range c.optionsFor(ports.ChatTurnSettings{Model: value}) {
			if mapped.ID != "model" {
				continue
			}
			value = mapped.Value
			if !choiceOffered(option.Choices, value) && mapped.Name != "" {
				for _, choice := range option.Choices {
					if strings.EqualFold(choice.Name, mapped.Name) {
						value = choice.Value
						break
					}
				}
			}
		}
	}
	if option.Type == ports.ChatConfigOptionBoolean {
		if value == "on" || value == "off" {
			return value, nil
		}
	} else if choiceOffered(option.Choices, value) {
		return value, nil
	}
	return "", fmt.Errorf("%w: %s_value_not_offered", ports.ErrAgentSettingsUnsupported, id)
}

// PrepareAgentSettings captures authoritative native choices without setters.
func (c *conversation) PrepareAgentSettings(ctx context.Context, source, target domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentSettingsProviderPlan{}, err
	}
	if source.Validate() != nil || target.Validate() != nil {
		return domain.AgentSettingsProviderPlan{}, ports.ErrAgentSettingsInvalid
	}
	catalog, err := c.settingsCatalog()
	if err != nil {
		return domain.AgentSettingsProviderPlan{}, err
	}
	before, permission := nativeSettings(catalog)
	plan := domain.AgentSettingsProviderPlan{Protocol: "acp", ProviderConversationID: c.ProviderConversationID(), Source: source, Target: target, Before: before, Permission: permission}
	if before.Model == "" || source.Model != target.Model && permission == "" {
		return plan, fmt.Errorf("%w: provider_preimage_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	for _, id := range []string{"model", "effort", "fast"} {
		if id != "model" && source.Model != target.Model {
			continue
		} // The selected model owns these choices.
		if _, err := c.targetOption(plan, id, false, catalog); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

func (c *conversation) checkSettingsPermission(plan domain.AgentSettingsProviderPlan, catalog []ports.ChatConfigOption) error {
	_, permission := nativeSettings(catalog)
	if permission != plan.Permission {
		return fmt.Errorf("%w: permission_dependency_changed", ports.ErrAgentSettingsConflict)
	}
	return nil
}

func (c *conversation) applySettingsOption(ctx context.Context, plan domain.AgentSettingsProviderPlan, id string, restoring bool) error {
	catalog, err := c.settingsCatalog()
	if err != nil {
		return err
	}
	value, err := c.targetOption(plan, id, restoring, catalog)
	if err != nil {
		return err
	}
	if value == "" {
		return nil
	}
	current, _ := nativeSettings(catalog)
	if settingsValue(current, id) != value {
		request := ports.ChatConfigOptionValue{Select: value}
		for _, option := range catalog {
			if option.ID == id && option.Type == ports.ChatConfigOptionBoolean {
				request = ports.ChatConfigOptionValue{Boolean: new(value == "on")}
				break
			}
		}
		result, err := c.SetConfigOption(ctx, id, request)
		if err != nil {
			return err
		}
		if len(result) == 0 {
			return fmt.Errorf("%w: provider_catalog_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
		catalog, err = c.settingsCatalog()
		if err != nil {
			return err
		}
		current, _ = nativeSettings(catalog)
		if settingsValue(current, id) != value {
			return fmt.Errorf("%w: provider_setting_unconfirmed", ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
	}
	if !restoring {
		return c.checkSettingsPermission(plan, catalog)
	}
	return nil
}

// ApplyAgentSettingsStep is deliberately separate from legacy turn settings.
// The runtime checkpoints before calling it, including before restoration.
func (c *conversation) ApplyAgentSettingsStep(ctx context.Context, plan domain.AgentSettingsProviderPlan, step domain.AgentSettingsCheckpoint, restoring bool) error {
	if plan.Protocol != "acp" || plan.ProviderConversationID != c.ProviderConversationID() {
		return ports.ErrAgentSettingsConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if restoring {
		if step != domain.AgentSettingsRollingBack {
			return ports.ErrAgentSettingsInvalid
		}
		if err := c.applySettingsOption(ctx, plan, "model", true); err != nil {
			return err
		}
		catalog, err := c.settingsCatalog()
		if err != nil {
			return err
		}
		_, permission := nativeSettings(catalog)
		if permission != plan.Permission {
			result, err := c.SetConfigOption(ctx, "mode", ports.ChatConfigOptionValue{Select: plan.Permission})
			if err != nil {
				return err
			}
			if len(result) == 0 {
				return fmt.Errorf("%w: permission_restore_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
			}
		}
		for _, id := range []string{"effort", "fast"} {
			if err := c.applySettingsOption(ctx, plan, id, true); err != nil {
				return err
			}
		}
		catalog, err = c.settingsCatalog()
		if err != nil {
			return err
		}
		return c.checkSettingsPermission(plan, catalog)
	}
	switch step {
	case domain.AgentSettingsApplyingModel:
		return c.applySettingsOption(ctx, plan, "model", false)
	case domain.AgentSettingsApplyingEffort:
		return c.applySettingsOption(ctx, plan, "effort", false)
	case domain.AgentSettingsApplyingFast:
		return c.applySettingsOption(ctx, plan, "fast", false)
	case domain.AgentSettingsVerifying:
		catalog, err := c.settingsCatalog()
		if err != nil {
			return err
		}
		current, _ := nativeSettings(catalog)
		for _, id := range []string{"model", "effort", "fast"} {
			value, err := c.targetOption(plan, id, false, catalog)
			if err != nil {
				return err
			}
			if value != "" && settingsValue(current, id) != value {
				return fmt.Errorf("%w: provider_setting_unconfirmed", ports.ErrAgentSettingsCapabilitiesUnavailable)
			}
		}
		return c.checkSettingsPermission(plan, catalog)
	default:
		return ports.ErrAgentSettingsInvalid
	}
}
