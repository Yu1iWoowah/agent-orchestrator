package agentsettings

import (
	"context"
	"fmt"
	"slices"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ValidateSpawn consumes the same qualified evidence exposed by Read. Clearing
// an incompatible inherited effort is permitted only when the model changed
// and the caller did not explicitly choose that effort.
func (s *Service) ValidateSpawn(ctx context.Context, id domain.SessionID, settings domain.AgentSettings, clearInheritedEffort bool) (domain.AgentSettings, error) {
	if err := settings.Validate(); err != nil {
		return settings, fmt.Errorf("%w: %w", ports.ErrAgentSettingsInvalid, err)
	}
	if settings.Model == "" && settings.Effort == "" && settings.FastMode == domain.AgentFastDefault {
		return settings, nil
	}
	snapshot, err := s.readCapabilities(ctx, id)
	if err != nil {
		return settings, err
	}
	if settings.FastMode != domain.AgentFastDefault && !snapshot.Harness.SupportsFastMode() {
		return settings, fmt.Errorf("%w: fast_unsupported", ports.ErrAgentSettingsUnsupported)
	}
	if snapshot.Capabilities.State != CapabilitySupported {
		return settings, fmt.Errorf("%w: %s", ports.ErrAgentSettingsCapabilitiesUnavailable, snapshot.Capabilities.Reason)
	}
	if s.catalogInputs == nil || snapshot.catalogFingerprint == "" {
		return settings, fmt.Errorf("%w: catalog_inputs_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	current, err := s.catalogInputs.CatalogFingerprint(ctx, string(snapshot.Harness), ports.AccountModelCatalogScope(snapshot.AccountID))
	if err != nil || current == "" {
		return settings, fmt.Errorf("%w: catalog_inputs_unverified", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	if current != snapshot.catalogFingerprint {
		return settings, fmt.Errorf("%w: catalog_stale", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	var selected *ModelCapabilities
	for i := range snapshot.Capabilities.Models {
		model := &snapshot.Capabilities.Models[i]
		if string(settings.Model) == model.Model || settings.Model == "" && model.IsDefault {
			selected = model
			break
		}
	}
	if selected == nil {
		if settings.Model == "" {
			return settings, fmt.Errorf("%w: provider_model_default_unknown", ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
		return settings, fmt.Errorf("%w: model_invalid", ports.ErrAgentSettingsInvalid)
	}
	if settings.Effort != "" && !slices.Contains(selected.Efforts, string(settings.Effort)) {
		if clearInheritedEffort {
			settings.Effort = ""
		} else {
			return settings, fmt.Errorf("%w: effort_unsupported", ports.ErrAgentSettingsUnsupported)
		}
	}
	if settings.FastMode != domain.AgentFastDefault && selected.Fast != CapabilitySupported {
		if selected.Fast == CapabilityUnsupported {
			return settings, fmt.Errorf("%w: fast_unsupported", ports.ErrAgentSettingsUnsupported)
		}
		return settings, fmt.Errorf("%w: fast_unknown", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	return settings, nil
}
