package agentsettings

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type pendingSettingsReader interface {
	PendingAgentSettingsOperation(context.Context, domain.SessionID) (domain.AgentSettingsOperation, bool, error)
}

// SetObservationSource wires a non-waking reader during daemon startup.
func (s *Service) SetObservationSource(source ports.AgentSettingsObservationSource) {
	s.observations = source
}

func (s *Service) readObservations(ctx context.Context, snapshot *Snapshot) error {
	snapshot.Effective = domain.UnknownAgentSettingsEffective("controller_unavailable")
	if err := s.readHistory(ctx, snapshot); err != nil {
		return err
	}
	if reader, ok := s.store.(pendingSettingsReader); ok {
		op, found, err := reader.PendingAgentSettingsOperation(ctx, snapshot.SessionID)
		if err != nil {
			return err
		}
		if found {
			snapshot.Pending = &op
		}
	}
	if snapshot.IsTerminated {
		snapshot.Effective = domain.UnknownAgentSettingsEffective("session_terminated")
		return nil
	}
	if snapshot.AccountID == "" {
		snapshot.Effective = domain.UnknownAgentSettingsEffective("account_unbound")
		return nil
	}
	if s.observations == nil {
		return nil
	}
	receipt, err := s.observations.ObserveAgentSettings(ctx, snapshot.SessionID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		snapshot.Effective = domain.UnknownAgentSettingsEffective("observation_unavailable")
		return nil
	}
	owner := receipt.Owner
	if !observationOwnerMatches(owner, snapshot.SessionAgentSettings) {
		snapshot.Effective = domain.UnknownAgentSettingsEffective("observation_owner_mismatch")
		return nil
	}
	// An owner switch racing the receipt cannot lend the old provider's evidence
	// to the replacement. This read does not mutate or wake either controller.
	current, err := s.store.SessionAgentSettings(ctx, snapshot.SessionID)
	if err != nil {
		return err
	}
	if current != snapshot.SessionAgentSettings {
		snapshot.Effective = domain.UnknownAgentSettingsEffective("observation_owner_changed")
		return nil
	}
	now := s.now()
	if turn := receipt.CurrentTurn; turn != nil {
		frameSnapshot := *turn
		if frameSnapshot.State == domain.AgentSettingKnown && (frameSnapshot.Settings == nil || frameSnapshot.Settings.Validate() != nil || frameSnapshot.CapturedAt == nil || frameSnapshot.CapturedAt.IsZero() || frameSnapshot.CapturedAt.After(now) || frameSnapshot.Revision != nil && *frameSnapshot.Revision > snapshot.Revision) {
			frameSnapshot = domain.AgentSettingsTurnSnapshot{State: domain.AgentSettingUnknown, ProviderTurnID: turn.ProviderTurnID, Reason: "dispatch_snapshot_untrusted"}
		}
		snapshot.CurrentTurn = &frameSnapshot
	}
	effective := receipt.Effective
	if effective.ObservedAt == nil || effective.ObservedAt.IsZero() {
		snapshot.Effective = domain.UnknownAgentSettingsEffective("provider_not_reported")
		return nil
	}
	if effective.ObservedAt.After(now) {
		snapshot.Effective = domain.UnknownAgentSettingsEffective("observation_time_untrusted")
		return nil
	}
	effective.Model = qualifiedField(effective.Model, "model")
	effective.Effort = qualifiedField(effective.Effort, "effort")
	effective.FastMode = qualifiedField(effective.FastMode, "fast")
	effective.Owner = &owner
	snapshot.Effective = effective
	return nil
}

func observationOwnerMatches(owner domain.AgentSettingsOwner, settings domain.SessionAgentSettings) bool {
	return owner.SessionID == settings.SessionID && owner.AccountID != "" && owner.AccountID == settings.AccountID && owner.ControllerGeneration != "" && owner.ControllerGeneration == settings.OwnerGeneration && owner.ProviderConversationID != "" && owner.ProviderConversationID == settings.ProviderConversationID && owner.BranchID != "" && owner.BranchID == settings.BranchID
}

func qualifiedField(value domain.AgentSettingObservation, field string) domain.AgentSettingObservation {
	if value.State == domain.AgentSettingUnknown || value.State == domain.AgentSettingNotProvided {
		value.Value = ""
		return value
	}
	valid := value.State == domain.AgentSettingKnown && value.Value != ""
	switch field {
	case "fast":
		valid = valid && (value.Value == "on" || value.Value == "off")
	case "model":
		valid = valid && value.Value != "default" && (domain.AgentSettings{Model: domain.AgentModelSelection(value.Value), FastMode: domain.AgentFastDefault}).Validate() == nil
	case "effort":
		valid = valid && value.Value != "default" && (domain.AgentSettings{Effort: domain.AgentEffortSelection(value.Value), FastMode: domain.AgentFastDefault}).Validate() == nil
	}
	if !valid {
		return domain.AgentSettingObservation{State: domain.AgentSettingUnknown, Reason: "provider_value_unverified"}
	}
	return value
}
