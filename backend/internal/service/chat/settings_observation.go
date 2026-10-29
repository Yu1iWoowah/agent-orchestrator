package chat

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type agentSettingsContextReader interface {
	SessionAgentSettings(context.Context, domain.SessionID) (domain.SessionAgentSettings, error)
}

func (s *Service) settingsAccountBeforeLaunch(ctx context.Context, id domain.SessionID) string {
	if reader, ok := s.store.(agentSettingsContextReader); ok {
		if settings, err := reader.SessionAgentSettings(ctx, id); err == nil {
			return settings.AccountID
		}
	}
	return "" // Missing launch identity makes receipts unknown, not device-owned.
}

func (c *Controller) observationOwner(account string) domain.AgentSettingsOwner {
	return domain.AgentSettingsOwner{SessionID: c.sessionID, AccountID: account, ControllerGeneration: c.Generation(), ProviderConversationID: c.ProviderConversationID(), BranchID: c.conversation.ActiveBranchID}
}

// newObservedBranchController binds identity before private edited-prompt
// dispatch as well as before publication. The account was captured before the
// replacement provider started, never derived from a later binding.
func (s *Service) newObservedBranchController(id domain.SessionID, source *Controller, conversation domain.ConversationRecord, generation string, provider ports.ChatConversation, account string) *Controller {
	c := newController(id, source.owner(), conversation, generation, source.harness, provider, s.store, s.activity, s.log, s.newID, s.now, s.onAccountChanged, s.onCodexCapacityChanged, s.reader)
	c.settingsObservationOwner = c.observationOwner(account)
	s.configureAgentSettingsController(c)
	return c
}

// ObserveAgentSettings reads an existing controller only. It never invokes the
// workingController path, reattaches a provider, or changes any preference.
func (s *Service) ObserveAgentSettings(ctx context.Context, id domain.SessionID) (domain.AgentSettingsObservation, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentSettingsObservation{}, err
	}
	c, err := s.Controller(id)
	if err != nil {
		return domain.AgentSettingsObservation{}, err
	}
	select {
	case <-c.stopped:
		return domain.AgentSettingsObservation{}, ErrNoController
	default:
	}
	c.mu.Lock()
	result := domain.AgentSettingsObservation{Owner: c.settingsObservationOwner, Effective: domain.UnknownAgentSettingsEffective("provider_not_reported")}
	if c.pendingTurnID != "" || c.dispatchingTurnID != "" {
		frame := c.settingsDispatchSnapshot
		if frame != nil && (frame.ProviderTurnID == c.pendingTurnID && c.pendingTurnID != "" || frame.TurnID == c.dispatchingTurnID && c.dispatchingTurnID != "") {
			frameSnapshot := *frame
			if frame.Owner != nil {
				owner := *frame.Owner
				frameSnapshot.Owner = &owner
			}
			if frame.Settings != nil {
				settings := *frame.Settings
				frameSnapshot.Settings = &settings
			}
			if frame.Revision != nil {
				revision := *frame.Revision
				frameSnapshot.Revision = &revision
			}
			if frame.CapturedAt != nil {
				at := *frame.CapturedAt
				frameSnapshot.CapturedAt = &at
			}
			result.CurrentTurn = &frameSnapshot
		} else {
			result.CurrentTurn = &domain.AgentSettingsTurnSnapshot{State: domain.AgentSettingUnknown, ProviderTurnID: c.pendingTurnID, Reason: "dispatch_snapshot_unavailable"}
		}
	}
	c.mu.Unlock()
	if c.conv.ProviderConversationID() != result.Owner.ProviderConversationID {
		return domain.AgentSettingsObservation{}, ports.ErrAgentSettingsConflict
	}
	if observer, ok := c.conv.(ports.ChatAgentSettingsObserver); ok {
		result.Effective = observer.AgentSettingsObservation()
	}
	if c.conv.ProviderConversationID() != result.Owner.ProviderConversationID {
		return domain.AgentSettingsObservation{}, ports.ErrAgentSettingsConflict
	}
	select {
	case <-c.stopped:
		return domain.AgentSettingsObservation{}, ErrNoController
	default:
	}
	return result, nil
}

func (c *Controller) captureSettingsDispatch(ctx context.Context, turnID string, wire ports.ChatTurnSettings) *domain.AgentSettingsTurnSnapshot {
	fast := wire.FastMode
	if wire.Fast {
		fast = domain.AgentFastOn
	}
	if fast == "" {
		fast = domain.AgentFastDefault
	}
	settings := domain.AgentSettings{Model: domain.AgentModelSelection(wire.Model), Effort: domain.AgentEffortSelection(wire.Effort), FastMode: fast}
	at := c.now()
	owner := c.settingsObservationOwner
	snapshot := &domain.AgentSettingsTurnSnapshot{State: domain.AgentSettingKnown, Source: "root", Owner: &owner, TurnID: turnID, Settings: &settings, CapturedAt: &at, Reason: "settings_revision_unavailable"}
	if reader, ok := c.store.(agentSettingsContextReader); ok {
		current, err := reader.SessionAgentSettings(ctx, c.sessionID)
		if err == nil && !current.IsTerminated && owner.AccountID != "" && owner.ProviderConversationID != "" && owner.BranchID != "" && current.Settings == settings && current.AccountID == owner.AccountID && current.OwnerGeneration == c.generation && current.ProviderConversationID == owner.ProviderConversationID && current.BranchID == owner.BranchID {
			revision := current.Revision
			snapshot.Revision, snapshot.Reason = &revision, ""
		}
	}
	return snapshot
}
