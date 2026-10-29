package codexappserver

import (
	"context"
	"fmt"
	"slices"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.ChatAgentSettingsController = (*conversation)(nil)

// PrepareAgentSettings resolves explicit reset wire from current provider inputs.
func (c *conversation) PrepareAgentSettings(ctx context.Context, source, target domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	if source.Validate() != nil || target.Validate() != nil {
		return domain.AgentSettingsProviderPlan{}, ports.ErrAgentSettingsInvalid
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	var response struct {
		Config *struct {
			Model  string `json:"model"`
			Effort string `json:"model_reasoning_effort"`
			Tier   string `json:"service_tier"`
		} `json:"config"`
	}
	if err := c.conn.request(ctx, "config/read", map[string]any{}, &response); err != nil || response.Config == nil {
		return domain.AgentSettingsProviderPlan{}, fmt.Errorf("%w: provider_baseline_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	models, err := listModels(ctx, c.conn)
	if err != nil {
		return domain.AgentSettingsProviderPlan{}, fmt.Errorf("%w: provider_models_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	baselineModel := response.Config.Model
	if baselineModel == "" {
		for _, model := range models {
			if model.Default {
				if baselineModel != "" {
					return domain.AgentSettingsProviderPlan{}, fmt.Errorf("%w: provider_default_ambiguous", ports.ErrAgentSettingsCapabilitiesUnavailable)
				}
				baselineModel = model.ID
			}
		}
	}
	resolve := func(selection domain.AgentSettings) (domain.AgentSettingsNativeValues, error) {
		modelID := string(selection.Model)
		if modelID == "" {
			modelID = baselineModel
		}
		var selected *ports.ChatModel
		for i := range models {
			if models[i].ID == modelID {
				selected = &models[i]
				break
			}
		}
		if selected == nil {
			return domain.AgentSettingsNativeValues{}, fmt.Errorf("%w: provider_model_default_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
		effort := string(selection.Effort)
		if effort == "" {
			effort = response.Config.Effort
			if effort == "" {
				effort = selected.DefaultEffort
			}
		}
		if effort == "" || !slices.Contains(selected.Efforts, effort) && effort != selected.DefaultEffort {
			return domain.AgentSettingsNativeValues{}, fmt.Errorf("%w: provider_effort_default_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
		tier := response.Config.Tier
		if tier == "" {
			tier = "default"
		}
		switch selection.FastMode {
		case domain.AgentFastOn:
			if selected.FastTier == "" {
				return domain.AgentSettingsNativeValues{}, fmt.Errorf("%w: fast_tier_not_offered", ports.ErrAgentSettingsUnsupported)
			}
			tier = selected.FastTier
		case domain.AgentFastOff:
			tier = "default"
		}
		return domain.AgentSettingsNativeValues{Model: modelID, Effort: effort, Fast: tier}, nil
	}
	before, err := resolve(source)
	if err != nil {
		return domain.AgentSettingsProviderPlan{}, err
	}
	after, err := resolve(target)
	if err != nil {
		return domain.AgentSettingsProviderPlan{}, err
	}
	return domain.AgentSettingsProviderPlan{Protocol: "codex-app-server", ProviderConversationID: c.ProviderConversationID(), Source: source, Target: target, Before: before, After: after, Permission: string(c.launchMode)}, nil
}

// PrepareAgentSettingsRecovery proves current durable wire independently of the
// failed target and old binary/account baseline. Codex applies it on turn/start.
func (c *conversation) PrepareAgentSettingsRecovery(ctx context.Context, settings domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	return c.PrepareAgentSettings(ctx, settings, settings)
}

// Codex changes settings on turn/start. Staging does not send a turn or claim
// effective provider state; the controller commits its revision before dispatch.
func (c *conversation) ApplyAgentSettingsStep(ctx context.Context, plan domain.AgentSettingsProviderPlan, step domain.AgentSettingsCheckpoint, restoring bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if plan.Protocol != "codex-app-server" || plan.ProviderConversationID != c.ProviderConversationID() || plan.After.Model == "" || plan.After.Effort == "" || plan.After.Fast == "" {
		return ports.ErrAgentSettingsConflict
	}
	if restoring {
		if step != domain.AgentSettingsRollingBack || plan.Before.Model == "" || plan.Before.Effort == "" || plan.Before.Fast == "" {
			return ports.ErrAgentSettingsInvalid
		}
		plan.Target, plan.After = plan.Source, plan.Before
		c.settingsPlan = &plan
		return nil
	}
	switch step {
	case domain.AgentSettingsApplyingModel, domain.AgentSettingsApplyingEffort, domain.AgentSettingsApplyingFast:
		return nil
	case domain.AgentSettingsVerifying:
		c.settingsPlan = &plan
		return nil
	default:
		return ports.ErrAgentSettingsInvalid
	}
}

func turnSelection(settings ports.ChatTurnSettings) domain.AgentSettings {
	fast := settings.FastMode
	if settings.Fast {
		fast = domain.AgentFastOn
	}
	if fast == "" {
		fast = domain.AgentFastDefault
	}
	return domain.AgentSettings{Model: domain.AgentModelSelection(settings.Model), Effort: domain.AgentEffortSelection(settings.Effort), FastMode: fast}
}
