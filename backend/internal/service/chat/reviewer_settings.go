package chat

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
)

// SetReviewerSettingsControl routes legacy reviewer choices through stable next-pass pins.
func (s *Service) SetReviewerSettingsControl(control ports.ReviewerChatSettingsControl) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reviewerSettingsControl = control
}

func (s *Service) reviewerSettings() ports.ReviewerChatSettingsControl {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reviewerSettingsControl
}

func reviewerConversationSettings(settings domain.ConversationSettings, next domain.AgentSettings) domain.ConversationSettings {
	settings.Model, settings.ReasoningEffort = string(next.Model), string(next.Effort)
	settings.AgentFastMode, settings.FastMode = next.FastMode, next.FastMode == domain.AgentFastOn
	return settings
}

func (s *Service) projectReviewerSettings(ctx context.Context, reviewID string, settings domain.ConversationSettings) (domain.ConversationSettings, error) {
	control := s.reviewerSettings()
	if control == nil {
		return settings, nil
	}
	selected, err := control.ReviewerChatSettings(ctx, reviewID)
	if err != nil {
		return settings, agentsettings.SettingsError(err)
	}
	if selected != nil {
		settings = reviewerConversationSettings(settings, *selected)
	}
	return settings, nil
}

func (s *Service) pinReviewerTurnSettings(ctx context.Context, reviewID string, live, requested domain.ConversationSettings) (domain.ConversationSettings, error) {
	control := s.reviewerSettings()
	if control == nil {
		return live, agentsettings.SettingsError(ports.ErrAgentSettingsUnsupported)
	}
	previous, err := s.projectReviewerSettings(ctx, reviewID, live)
	if err != nil {
		return live, err
	}
	patch := selectionPatch(previous, requested)
	if patch == (domain.AgentSettingsPatch{}) {
		return previous, nil
	}
	next, err := control.PinReviewerChatSettings(ctx, reviewID, patch)
	if err != nil {
		return previous, agentsettings.SettingsError(err)
	}
	return reviewerConversationSettings(live, next), nil
}
