package chat

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SetAgentSettingsMutator routes supported legacy menus through the shared scheduler.
func (s *Service) SetAgentSettingsMutator(mutator ports.ChatAgentSettingsMutator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settingsMutator = mutator
	for _, c := range s.ownerControllers {
		c.mu.Lock()
		c.settingsMutator = mutator
		c.mu.Unlock()
	}
}

func (c *Controller) agentSettingsMutator() ports.ChatAgentSettingsMutator {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reviewID != "" || !c.harness.SupportsFastMode() {
		return nil
	}
	return c.settingsMutator
}

func normalizedConversationFast(previous, settings domain.ConversationSettings) domain.AgentFastMode {
	if settings.FastMode {
		return domain.AgentFastOn
	}
	if settings.AgentFastMode == domain.AgentFastOn {
		return domain.AgentFastOff
	}
	if settings.AgentFastMode != "" {
		return settings.AgentFastMode
	}
	if previous.FastMode || previous.AgentFastMode == domain.AgentFastOn {
		return domain.AgentFastOff
	}
	if previous.AgentFastMode != "" {
		return previous.AgentFastMode
	}
	return domain.AgentFastDefault
}

func selectionPatch(previous, next domain.ConversationSettings) domain.AgentSettingsPatch {
	p := domain.AgentSettingsPatch{}
	if next.Model != previous.Model {
		p.Model = new(domain.AgentModelSelection(next.Model))
	}
	if next.ReasoningEffort != previous.ReasoningEffort {
		p.Effort = new(domain.AgentEffortSelection(next.ReasoningEffort))
	}
	fast := normalizedConversationFast(previous, next)
	if fast != normalizedConversationFast(previous, previous) {
		p.FastMode = &fast
	}
	return p
}

func settingsIdentityPresent(ctx context.Context) bool {
	caller := ports.AgentSettingsCallerFromContext(ctx)
	return caller.SessionID != "" || caller.Grant != ""
}

// Non-selection controls keep their manual semantics, without overwriting the
// model/effort/fast projection committed by a concurrent scheduler transaction.
func (c *Controller) setManualControls(ctx context.Context, settings domain.ConversationSettings) error {
	previous := c.Settings()
	if previous.ApprovalMode == settings.ApprovalMode && previous.PlanMode == settings.PlanMode && previous.OpenCodeMode == settings.OpenCodeMode && previous.AutoCompactWindow == settings.AutoCompactWindow {
		return nil
	}
	if settingsIdentityPresent(ctx) {
		return apierr.Forbidden("AGENT_SETTINGS_PERMISSION_DENIED", "Settings grants cannot change manual permission or plan controls")
	}
	writer, ok := c.store.(interface {
		SetConversationManualControls(context.Context, string, domain.ConversationSettings, time.Time) error
	})
	if !ok {
		return ports.ErrAgentSettingsUnsupported
	}
	if err := writer.SetConversationManualControls(ctx, c.conversation.ID, settings, c.now()); err != nil {
		return err
	}
	c.mu.Lock()
	c.settings.ApprovalMode = settings.ApprovalMode
	c.settings.PlanMode = settings.PlanMode
	c.settings.OpenCodeMode = settings.OpenCodeMode
	c.settings.AutoCompactWindow = settings.AutoCompactWindow
	c.mu.Unlock()
	return nil
}

func (c *Controller) scheduleLegacySettings(ctx context.Context, settings domain.ConversationSettings, mutator ports.ChatAgentSettingsMutator) error {
	previous, err := c.projectedLegacySettings(ctx, c.Settings())
	if err != nil {
		return err
	}
	if settingsIdentityPresent(ctx) && (previous.ApprovalMode != settings.ApprovalMode || previous.PlanMode != settings.PlanMode || previous.OpenCodeMode != settings.OpenCodeMode) {
		return apierr.Forbidden("AGENT_SETTINGS_PERMISSION_DENIED", "Settings grants cannot change manual permission or plan controls")
	}
	patch := selectionPatch(previous, settings)
	if patch != (domain.AgentSettingsPatch{}) {
		if _, err := mutator.ChangeAgentSettings(ctx, c.sessionID, patch, c); err != nil {
			return err
		}
	}
	return c.setManualControls(ctx, settings)
}

func (c *Controller) scheduleLegacyConfigOption(ctx context.Context, id string, value ports.ChatConfigOptionValue, release func()) (bool, error) {
	mutator := c.agentSettingsMutator()
	if mutator == nil {
		if settingsIdentityPresent(ctx) {
			return true, apierr.Forbidden("AGENT_SETTINGS_TARGET_DENIED", "This target has no managed settings scheduler")
		}
		return false, nil
	}
	p := domain.AgentSettingsPatch{}
	configID := id
	if configID != "model" && configID != "effort" && configID != "fast" {
		if configurer, ok := c.conv.(ports.ChatConfigOptionController); ok {
			options, err := configurer.ListConfigOptions(ctx)
			if err != nil {
				return true, err
			}
			for _, option := range options {
				if option.ID == id {
					if option.Category == "model" {
						configID = "model"
					}
					if option.Category == "thought_level" {
						configID = "effort"
					}
				}
			}
		}
	}
	switch configID {
	case "model":
		model := value.Select
		if c.harness == domain.HarnessClaudeCode && model == "default" {
			model = ""
		}
		p.Model = new(domain.AgentModelSelection(model))
	case "effort":
		effort := value.Select
		if c.harness == domain.HarnessClaudeCode && effort == "default" {
			effort = ""
		}
		p.Effort = new(domain.AgentEffortSelection(effort))
	case "fast":
		fast := domain.AgentFastMode(value.Select)
		if value.Boolean != nil {
			fast = domain.AgentFastOff
			if *value.Boolean {
				fast = domain.AgentFastOn
			}
		}
		p.FastMode = &fast
	default:
		if settingsIdentityPresent(ctx) {
			return true, apierr.Forbidden("AGENT_SETTINGS_PERMISSION_DENIED", "Settings grants cannot change other provider controls")
		}
		return false, nil
	}
	release()
	_, err := mutator.ChangeAgentSettings(ctx, c.sessionID, p, c)
	return true, err
}
