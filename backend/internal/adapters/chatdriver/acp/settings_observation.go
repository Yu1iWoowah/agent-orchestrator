package acp

import (
	"context"
	"strconv"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (c *conversation) AgentSettingsEffortOptions() *domain.AgentSettingsEffortOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.agentSettingsDefaults == nil || c.settingsObservedAt.IsZero() || c.settingsReceiptSessionID == "" || c.settingsReceiptSessionID != c.sessionID {
		return nil
	}
	native, permission := nativeSettings(c.settingsReceipts)
	if native.Model == "" || !offeredNativeSetting(c.settingsReceipts, "model", native.Model) {
		return nil
	}
	for _, option := range c.settingsReceipts {
		if option.ID != "effort" || !choiceOffered(option.Choices, native.Effort) {
			continue
		}
		if permission == "" || !offeredNativeSetting(c.settingsReceipts, "mode", permission) {
			return &domain.AgentSettingsEffortOptions{ObservedAt: c.settingsObservedAt, Model: native.Model}
		}
		result := &domain.AgentSettingsEffortOptions{ObservedAt: c.settingsObservedAt, Model: native.Model, Permission: permission}
		if reset := c.agentSettingsDefaults["effort"]; reset != "" && choiceOffered(option.Choices, reset) {
			result.ResetSelector = reset
		}
		for _, choice := range option.Choices {
			effort := domain.AgentSettings{Effort: domain.AgentEffortSelection(choice.Value), FastMode: domain.AgentFastDefault}
			if choice.Value != "" && choice.Value != "default" && effort.Validate() == nil {
				result.Efforts = append(result.Efforts, choice.Value)
			}
		}
		return result
	}
	return nil
}

func offeredNativeSetting(options []ports.ChatConfigOption, id, value string) bool {
	for _, option := range options {
		if option.ID == id {
			return choiceOffered(option.Choices, value)
		}
	}
	return false
}

// ReconciliationPlanEvidence reports a same-session provider option receipt. A
// setter acknowledgement alone never updates this receipt.
func (c *conversation) ReconciliationPlanEvidence(ctx context.Context, owner domain.AgentSettingsOwner, plan *domain.AgentSettingsProviderPlan) (ports.AgentSettingsReconciliationEvidence, error) {
	var proof ports.AgentSettingsReconciliationEvidence
	if err := ctx.Err(); err != nil {
		return proof, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.agentSettingsDefaults == nil || c.settingsObservedAt.IsZero() || c.settingsReceiptSessionID == "" || c.settingsReceiptSessionID != c.sessionID || owner.ProviderConversationID != c.reportedSessionID {
		return proof, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	native, permission := nativeSettings(c.settingsReceipts)
	if native.Model == "" || permission == "" {
		return proof, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	if plan != nil {
		if plan.ProviderConversationID != owner.ProviderConversationID || plan.Protocol != "acp" {
			return proof, ports.ErrAgentSettingsConflict
		}
		if err := c.verifySettingsCatalog(*plan, c.settingsReceipts); err != nil {
			return proof, err
		}
	}
	return ports.AgentSettingsReconciliationEvidence{
		Kind: "provider_current", Owner: owner, ProviderEpoch: owner.ControllerGeneration,
		SourceRef: strconv.FormatInt(c.settingsObservedAt.UnixNano(), 10), Native: &native,
		Permission: permission, PermissionVerified: true,
	}, nil
}

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
