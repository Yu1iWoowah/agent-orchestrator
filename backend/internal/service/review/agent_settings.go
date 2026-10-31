package review

import (
	"context"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/reqid"
	reviewcore "github.com/aoagents/agent-orchestrator/backend/internal/review"
)

var _ ports.ReviewerChatSettingsControl = (*Service)(nil)

// ReviewerChatSettings reads the durable next-pass preference for reviewer menus.
func (s *Service) ReviewerChatSettings(ctx context.Context, id string) (*domain.AgentSettings, error) {
	state, err := s.ReviewerAgentSettings(ctx, id)
	return state.Settings, err
}

// PinReviewerChatSettings pins a local menu choice without changing an active pass.
func (s *Service) PinReviewerChatSettings(ctx context.Context, id string, patch domain.AgentSettingsPatch) (domain.AgentSettings, error) {
	state, err := s.engine.ReviewerAgentSettings(ctx, id)
	if err != nil {
		return domain.AgentSettings{}, err
	}
	op, err := s.engine.ChangeReviewerAgentSettings(ctx, id, reviewcore.ReviewerSettingsChange{BaseRevision: state.Revision, Patch: patch, IdempotencyKey: uuid.NewString(), RequestID: reqid.FromContext(ctx)})
	return op.Target, err
}

// SetReviewerSettingsAuthorizer enables reviewer control only after the
// daemon's settings-purpose grant authority has been installed.
func (s *Service) SetReviewerSettingsAuthorizer(authorizer ports.ReviewerSettingsAuthorizer) {
	s.engine.SetReviewerSettingsAuthorizer(authorizer)
}

// ReviewerAgentSettings reads caller-authorized stable reviewer preferences.
func (s *Service) ReviewerAgentSettings(ctx context.Context, id string) (domain.ReviewerAgentSettingsState, error) {
	return s.engine.ReadReviewerAgentSettingsForCaller(ctx, id)
}

// ChangeReviewerAgentSettings validates and pins a revision-qualified reviewer selection.
func (s *Service) ChangeReviewerAgentSettings(ctx context.Context, id string, req reviewcore.ReviewerSettingsChange) (domain.ReviewerAgentSettingsOperation, error) {
	return s.engine.ChangeReviewerAgentSettings(ctx, id, req)
}
