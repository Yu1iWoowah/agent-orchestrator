package acp

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AgentSettingsObservation uses only the last authoritative provider receipt.
// Optimistic UI projection and spawn preferences are deliberately separate.
func (c *conversation) AgentSettingsObservation() domain.AgentSettingsEffective {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return domain.UnknownAgentSettingsEffective("controller_unavailable")
	}
	if c.settingsObservedAt.IsZero() {
		return domain.UnknownAgentSettingsEffective("provider_not_reported")
	}
	if c.settingsReceiptSessionID == "" || c.settingsReceiptSessionID != c.sessionID {
		return domain.UnknownAgentSettingsEffective("provider_branch_unverified")
	}
	result := domain.UnknownAgentSettingsEffective("provider_not_reported")
	observed := c.settingsObservedAt
	result.ObservedAt, result.Source = &observed, "provider_config"
	result.FastMode = domain.AgentSettingObservation{State: domain.AgentSettingNotProvided, Reason: "provider_not_offered"}
	for _, option := range c.settingsReceipts {
		value := domain.AgentSettingObservation{State: domain.AgentSettingKnown, Value: option.Current.Select}
		if value.Value == "" || value.Value == "default" {
			value = domain.AgentSettingObservation{State: domain.AgentSettingUnknown, Reason: "provider_default_unresolved"}
		}
		switch {
		case option.ID == "model" || option.Category == "model":
			result.Model = value
		case option.ID == "effort" || option.Category == "thought_level":
			result.Effort = value
		case option.ID == "fast":
			if option.Type == ports.ChatConfigOptionBoolean && option.Current.Boolean != nil {
				value = domain.AgentSettingObservation{State: domain.AgentSettingKnown, Value: "off"}
				if *option.Current.Boolean {
					value.Value = "on"
				}
			}
			if value.Value != "on" && value.Value != "off" {
				value = domain.AgentSettingObservation{State: domain.AgentSettingUnknown, Reason: "provider_not_reported"}
			}
			result.FastMode = value
		}
	}
	return result
}

func (c *conversation) invalidateSettingsReceipt() {
	c.mu.Lock()
	c.settingsObservedAt = time.Time{}
	c.settingsReceipts = nil
	c.settingsReceiptSessionID = ""
	c.mu.Unlock()
}
