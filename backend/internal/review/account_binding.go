package review

import (
	"context"
	"slices"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// reviewerAccountContext is established after the execution row exists and
// before any native preflight/launch. It never borrows the worker's binding.
func (e *Engine) reviewerAccountContext(ctx context.Context, worker domain.SessionRecord, review domain.Review, override string) (context.Context, error) {
	if e.accounts == nil {
		return ctx, nil
	}
	var config domain.ProjectConfig
	if e.projects != nil && worker.ProjectID != "" {
		project, found, err := e.projects.GetProject(ctx, string(worker.ProjectID))
		if err != nil {
			return ctx, err
		}
		if found {
			config = project.Config
		}
	}
	selected, err := e.accounts.ResolveReviewerAccount(ctx, review.ID, ports.AgentAccountResolutionRequest{
		Harness: domain.AgentHarness(review.Harness), Role: domain.AgentAccountRoleReviewer,
		ProjectConfig: config, AccountID: override,
	})
	if err != nil {
		return ctx, err
	}
	return ports.WithAgentAccount(ctx, selected), nil
}

// ReviewerModels is the same account-scoped catalog used by worker preflight.
type ReviewerModels interface {
	ModelCatalog(context.Context, string, string, bool) (ports.AgentModelCatalog, error)
}

func (e *Engine) validateReviewerModels(ctx context.Context, harness domain.ReviewerHarness, config domain.AgentConfig) error {
	if e.models == nil || config.Model == "" && config.Effort == "" {
		return nil
	}
	catalog, err := e.models.ModelCatalog(ctx, string(harness), "", true)
	if err != nil {
		return err
	}
	for _, model := range catalog.Models {
		if model.ID == config.Model || config.Model == "" && model.IsDefault {
			if config.Effort != "" && !slices.Contains(model.Efforts, config.Effort) {
				return ports.ErrAgentSettingsUnsupported
			}
			return nil
		}
	}
	if catalog.AllowCustom && config.Effort == "" {
		return nil
	}
	return ports.ErrAgentSettingsCapabilitiesUnavailable
}

func (e *Engine) reviewerSurface(ctx context.Context, review domain.Review) domain.ReviewerSurface {
	surface := reviewerSurface(review)
	store, ok := e.store.(ports.ReviewerAgentAccountStore)
	if !ok || surface.ReviewID == "" {
		return surface
	}
	p, supported := domain.AgentAccountProviderFor(domain.AgentHarness(review.Harness))
	if !supported {
		return surface
	}
	binding, found, err := store.GetReviewerAgentAccount(ctx, review.ID, p)
	if err != nil || !found {
		return surface
	}
	view := domain.AgentAccountBindingView{AccountID: binding.AccountID, Provider: binding.Provider, Source: binding.Source}
	if reader, ok := e.accounts.(interface {
		DescribeAccountBinding(context.Context, domain.ResolvedAgentAccount) (domain.AgentAccountBindingView, error)
	}); ok {
		if observed, err := reader.DescribeAccountBinding(ctx, binding); err == nil {
			view = observed
		}
	}
	surface.AccountBinding = &view
	return surface
}
