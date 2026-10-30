package ports

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ReviewerAgentSettingsStore separates stable reviewer preferences and audit
// from worker settings. Commit must reject an active pass atomically.
type ReviewerAgentSettingsStore interface {
	ReviewerAgentSettings(context.Context, string) (domain.ReviewerAgentSettingsState, error)
	InitializeReviewerAgentSettings(context.Context, string, domain.AgentSettings, time.Time) (domain.ReviewerAgentSettingsState, error)
	CommitReviewerAgentSettings(context.Context, domain.ReviewerAgentSettingsOperation) (domain.ReviewerAgentSettingsOperation, error)
	ReviewerAgentSettingsOperationByKey(context.Context, string, string) (domain.ReviewerAgentSettingsOperation, bool, error)
}

// ReviewerSettingsAuthorization contains server-verified attribution, never
// the transport grant or provider credentials.
type ReviewerSettingsAuthorization struct {
	Actor          domain.AgentSettingsActor
	Authority      domain.AgentSettingsAuthority
	PolicyRevision string
}

// ReviewerSettingsAuthorizer verifies the caller and reviewer-specific project policy.
type ReviewerSettingsAuthorizer interface {
	AuthorizeReviewerSelection(context.Context, domain.SessionRecord, domain.Review, domain.AgentSettings) (ReviewerSettingsAuthorization, error)
}

// ReviewerChatSettingsControl adapts legacy reviewer menus to next-pass pins.
// It never applies a change to the live pass's conversation/controller.
type ReviewerChatSettingsControl interface {
	ReviewerChatSettings(context.Context, string) (*domain.AgentSettings, error)
	PinReviewerChatSettings(context.Context, string, domain.AgentSettingsPatch) (domain.AgentSettings, error)
}
