package review

import (
	"context"
	"slices"
	"time"

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
	CatalogFingerprint(context.Context, string, string) (string, error)
}

func (e *Engine) validateReviewerModels(ctx context.Context, harness domain.ReviewerHarness, config domain.AgentConfig) error {
	_, err := e.reviewerModelProof(ctx, harness, config, false)
	return err
}

func (e *Engine) reviewerModelProof(ctx context.Context, harness domain.ReviewerHarness, config domain.AgentConfig, required bool) (string, error) {
	if err := e.validateReviewerFields(ctx, harness, config, required); err != nil {
		return "", err
	}
	explicitFast := config.FastMode == domain.AgentFastOn || config.FastMode == domain.AgentFastOff
	if explicitFast && !domain.AgentHarness(harness).SupportsFastMode() {
		return "", ports.ErrAgentSettingsUnsupported
	}
	if e.models == nil {
		if explicitFast || required {
			return "", ports.ErrAgentSettingsCapabilitiesUnavailable
		}
		return "", nil
	}
	if !required && config.Model == "" && config.Effort == "" && !explicitFast {
		return "", nil
	}
	scope := ""
	if account, bound := ports.AgentAccountFromContext(ctx); bound {
		scope = ports.AccountModelCatalogScope(account.AccountID)
	}
	fingerprint, err := e.models.CatalogFingerprint(ctx, string(harness), scope)
	if err != nil || fingerprint == "" {
		return "", ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	catalog, err := e.models.ModelCatalog(ctx, string(harness), scope, false)
	if err != nil || catalog.InputFingerprint != fingerprint || validateReviewerCatalog(catalog, config, required || explicitFast, time.Now()) != nil {
		catalog, err = e.models.ModelCatalog(ctx, string(harness), scope, true)
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	if catalog.InputFingerprint != fingerprint {
		return "", ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	if err := validateReviewerCatalog(catalog, config, required || explicitFast, time.Now()); err != nil {
		return "", err
	}
	current, err := e.models.CatalogFingerprint(ctx, string(harness), scope)
	if err != nil || current != fingerprint {
		return "", ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	return fingerprint, nil
}

func (e *Engine) validateReviewerFields(ctx context.Context, harness domain.ReviewerHarness, config domain.AgentConfig, typed bool) error {
	if e.agents == nil {
		// The primary adapters implement these fields. Other adapters require
		// their own declaration; a catalog variant alone is not an effort wire.
		if config.Effort != "" && harness != domain.ReviewerCodex && harness != domain.ReviewerClaudeCode {
			return ports.ErrAgentSettingsUnsupported
		}
		return nil
	}
	agent, ok := e.agents.Agent(domain.AgentHarness(harness))
	if !ok {
		return ports.ErrAgentSettingsUnsupported
	}
	spec, err := agent.GetConfigSpec(ctx)
	if err != nil {
		return ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	fields := map[string]bool{}
	for _, field := range spec.Fields {
		fields[field.Key] = true
	}
	modelSupported := fields["model"]
	// Older Amp preferences used the model slot for its native mode picker.
	// Preserve that launch compatibility; never qualify it as typed effort/fast.
	if !typed && harness == domain.ReviewerAmp && fields["mode"] {
		modelSupported = true
	}
	if config.Model != "" && !modelSupported || config.Effort != "" && !fields["effort"] {
		return ports.ErrAgentSettingsUnsupported
	}
	return nil
}

func validateReviewerCatalog(catalog ports.AgentModelCatalog, config domain.AgentConfig, requireSuccess bool, now time.Time) error {
	if requireSuccess && (catalog.LastSuccessAt == nil || domain.AgentModelCatalogNeedsRevalidation(*catalog.LastSuccessAt, now)) {
		return ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	if catalog.Stale || catalog.RefreshError != "" || catalog.RefreshState != "" && catalog.RefreshState != "idle" {
		return ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	for _, model := range catalog.Models {
		if model.ID == config.Model || config.Model == "" && model.IsDefault {
			if config.Effort != "" && !slices.Contains(model.Efforts, config.Effort) {
				return ports.ErrAgentSettingsUnsupported
			}
			if config.FastMode == domain.AgentFastOn && !model.SupportsFast {
				return ports.ErrAgentSettingsUnsupported
			}
			return nil
		}
	}
	if catalog.AllowCustom && config.Effort == "" && config.FastMode != domain.AgentFastOn {
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
