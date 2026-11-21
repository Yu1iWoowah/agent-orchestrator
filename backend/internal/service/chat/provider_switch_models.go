package chat

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type switchModelCatalog interface {
	ModelCatalog(context.Context, string, string, bool) (ports.AgentModelCatalog, error)
}

// SetSwitchModelCatalog connects the model preflight before serving requests.
func (s *Service) SetSwitchModelCatalog(catalog switchModelCatalog) {
	s.switchModels = catalog
}

func (s *Service) checkSwitchPicks(ctx context.Context, request *domain.ProviderSwitchRequest) ([]string, error) {
	if s.switchModels == nil || (request.Model == "" && request.Effort == "") {
		return nil, nil
	}
	lookup, cancel := context.WithTimeout(ctx, switchCatalogLookupLimit)
	defer cancel()
	catalog, err := s.switchModels.ModelCatalog(lookup, string(request.TargetHarness), "", false)
	if err != nil || len(catalog.Models) == 0 {
		return nil, nil //nolint:nilerr // An unreadable catalog must not block a switch.
	}
	target := request.TargetHarness
	var warnings []string
	var picked *ports.AgentModelInfo
	if request.Model != "" {
		base, _, _ := strings.Cut(strings.ToLower(request.Model), "[")
		alias := false
		for i := range catalog.Models {
			id := strings.ToLower(catalog.Models[i].ID)
			if strings.EqualFold(id, request.Model) {
				request.Model = catalog.Models[i].ID
			}
			if strings.EqualFold(id, request.Model) || id == base {
				picked = &catalog.Models[i]
				break
			}
			alias = alias || slices.Contains(strings.Split(id, "-"), base)
		}
		switch {
		case picked != nil || alias:
		case catalog.CustomModelEntry != ports.CustomModelEntryDirect:
			return nil, &domain.ProviderSwitchError{Kind: domain.ProviderSwitchRefusalUnsupported,
				Detail: fmt.Sprintf("%s is not one of %s's models: %s", request.Model, target, strings.Join(catalogModelIDs(catalog), ", "))}
		default:
			warnings = append(warnings, fmt.Sprintf("%s is not in %s's model list; the agent may refuse it", request.Model, target))
		}
	}
	if request.Effort == "" {
		return warnings, nil
	}
	allowed, name := catalogEfforts(catalog), string(target)
	if picked != nil {
		allowed, name = picked.Efforts, picked.ID
	}
	if len(allowed) == 0 {
		return nil, &domain.ProviderSwitchError{Kind: domain.ProviderSwitchRefusalUnsupported,
			Detail: fmt.Sprintf("%s takes no reasoning effort", name)}
	}
	for _, effort := range allowed {
		if strings.EqualFold(effort, request.Effort) {
			request.Effort = effort
			return warnings, nil
		}
	}
	return nil, &domain.ProviderSwitchError{Kind: domain.ProviderSwitchRefusalUnsupported,
		Detail: fmt.Sprintf("effort %q is not one of %s", request.Effort, strings.Join(allowed, ", "))}
}

func catalogModelIDs(catalog ports.AgentModelCatalog) []string {
	ids := make([]string, 0, len(catalog.Models))
	for _, model := range catalog.Models {
		ids = append(ids, model.ID)
	}
	return ids
}

func catalogEfforts(catalog ports.AgentModelCatalog) []string {
	var efforts []string
	for _, model := range catalog.Models {
		for _, effort := range model.Efforts {
			if !slices.Contains(efforts, effort) {
				efforts = append(efforts, effort)
			}
		}
	}
	return efforts
}

var switchCatalogLookupLimit = 3 * time.Second
