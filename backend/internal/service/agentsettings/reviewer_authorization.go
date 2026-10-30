package agentsettings

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.ReviewerSettingsAuthorizer = (*Authorizer)(nil)

// AuthorizeReviewerSelection checks the reviewer's stable parent relationship,
// then reuses settings-purpose grant validation. Worker policy is never a
// fallback for reviewer permissions, and no account is selected here.
func (a *Authorizer) AuthorizeReviewerSelection(ctx context.Context, parent domain.SessionRecord, review domain.Review, target domain.AgentSettings) (ports.ReviewerSettingsAuthorization, error) {
	var result ports.ReviewerSettingsAuthorization
	if review.ID == "" || review.SessionID != parent.ID || review.ProjectID != parent.ProjectID || review.IsArchived || parent.IsTerminated || !review.Harness.IsKnown() {
		return result, apierr.Forbidden("AGENT_SETTINGS_TARGET_DENIED", "A current reviewer in the worker's project is required")
	}
	accepted, err := a.Authorize(ctx, ports.AgentSettingsCallerFromContext(ctx), parent)
	if err != nil {
		return result, err
	}
	result.Actor, result.Authority = accepted.Actor, accepted.Authority
	if accepted.Actor.Kind == domain.AgentSettingsActorLocal {
		result.PolicyRevision = "local_operator_unverified"
		return result, nil
	}
	project, found, err := a.store.GetProject(ctx, string(review.ProjectID))
	if err != nil {
		return result, err
	}
	policy := project.Config.AgentSettingsPolicy
	if !found || project.ID != string(review.ProjectID) || policy == nil || policy.Validate() != nil || !policy.AllowsReviewer(review.Harness, target) {
		return result, apierr.Forbidden("AGENT_SETTINGS_POLICY_DENIED", "Project reviewer settings policy does not allow this selection")
	}
	result.PolicyRevision, err = settingsPolicyRevision(policy)
	return result, err
}
