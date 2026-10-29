package project

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func accountConfigForMutation(ctx context.Context, current, proposed domain.ProjectConfig) (domain.ProjectConfig, error) {
	if !ports.IsRemoteAccountRequest(ctx) {
		return proposed, nil
	}
	if proposed.HasLocalAccountSettings() {
		return domain.ProjectConfig{}, apierr.Forbidden("AGENT_ACCOUNT_LOCAL_ONLY", "Local account settings can only be changed from this computer.")
	}
	return proposed.WithLocalAccountReferences(current), nil
}
