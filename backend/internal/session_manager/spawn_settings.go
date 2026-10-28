package sessionmanager

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/modelcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func resolveSpawnSettings(cfg ports.SpawnConfig, project domain.ProjectConfig) (ports.AgentConfig, domain.SpawnAgentSettingsResolution, bool, error) {
	base := effectiveAgentConfig(cfg.Harness, cfg.Kind, project)
	config := applySpawnAgentConfig(base, cfg.AgentConfig)
	if cfg.EffortOverride {
		config.Effort = cfg.AgentConfig.Effort
	}
	resolution := domain.SpawnAgentSettingsResolution{Sources: domain.AgentSettingsSources{Model: "provider", Effort: "provider", FastMode: "provider"}, Validation: "native-validation", Reason: "legacy_selection_unconfirmed"}
	role := roleOverride(cfg.Kind, project)
	matched := role.Harness == "" || role.Harness == cfg.Harness
	projectFast := project.AgentConfig.FastMode
	if !cfg.Harness.SupportsFastMode() {
		projectFast = ""
	}
	for _, field := range []struct {
		project, role, task string
		source              *string
	}{
		{project.AgentConfig.Model, role.AgentConfig.Model, cfg.AgentConfig.Model, &resolution.Sources.Model},
		{project.AgentConfig.Effort, role.AgentConfig.Effort, cfg.AgentConfig.Effort, &resolution.Sources.Effort},
		{string(projectFast), string(role.AgentConfig.FastMode), string(cfg.AgentConfig.FastMode), &resolution.Sources.FastMode},
	} {
		if field.project != "" {
			*field.source = "project"
		}
		if matched && field.role != "" {
			*field.source = "role"
		}
		if field.task != "" {
			*field.source = "request"
		}
	}
	if cfg.EffortOverride {
		resolution.Sources.Effort = "request"
	}
	settings := domain.AgentSettings{Model: domain.AgentModelSelection(config.Model), Effort: domain.AgentEffortSelection(config.Effort), FastMode: config.FastMode}
	if settings.FastMode == "" {
		settings.FastMode = domain.AgentFastDefault
	}
	modelChanged := cfg.AgentConfig.Model != "" && cfg.AgentConfig.Model != base.Model
	explicitEffort := cfg.EffortOverride || cfg.AgentConfig.Effort != ""
	if patch := cfg.AgentSettings; patch != nil {
		if patch.Model != nil && cfg.AgentConfig.Model != "" || patch.Effort != nil && (cfg.EffortOverride || cfg.AgentConfig.Effort != "") {
			return config, resolution, false, fmt.Errorf("%w: duplicate legacy and typed setting", ports.ErrAgentSettingsInvalid)
		}
		var err error
		settings, err = settings.Apply(*patch)
		if err != nil {
			return config, resolution, false, fmt.Errorf("%w: %w", ports.ErrAgentSettingsInvalid, err)
		}
		if patch.Model != nil {
			if *patch.Model == "" && modelcatalog.Base(string(cfg.Harness)).SelectionMode == ports.ModelSelectionModeList {
				config.Mode = ""
			}
			resolution.Sources.Model = "request"
			modelChanged = string(*patch.Model) != base.Model
		}
		if patch.Effort != nil {
			resolution.Sources.Effort = "request"
			explicitEffort = true
		}
		if patch.FastMode != nil {
			resolution.Sources.FastMode = "request"
		}
	}
	config.Model = string(settings.Model)
	config.Effort = string(settings.Effort)
	config.FastMode = settings.FastMode
	resolution.Settings = settings
	if project.AgentConfig.FastMode != "" && projectFast == "" && resolution.Sources.FastMode == "provider" {
		resolution.Sources.FastMode = "provider:project_fast_unsupported"
	}
	modelChanged = modelChanged || resolution.Sources.Effort == "project" && resolution.Sources.Model != "project" && settings.Model != domain.AgentModelSelection(project.AgentConfig.Model)
	return config, resolution, modelChanged && !explicitEffort, nil
}

// SetAgentSettingsPreflight completes startup wiring before requests are served.
func (m *Manager) SetAgentSettingsPreflight(preflight ports.AgentSettingsPreflight) {
	m.agentSettings = preflight
}

type spawnAgentSettingsStore interface {
	SaveSpawnAgentSettings(context.Context, domain.SessionID, domain.SpawnAgentSettingsResolution) error
}

func (m *Manager) validateSpawnSettings(ctx context.Context, id domain.SessionID, cfg *ports.SpawnConfig, resolution *domain.SpawnAgentSettingsResolution, strict, clearEffort bool) error {
	if m.agentAccounts != nil {
		account, err := m.agentAccounts.SessionAccountID(ctx, id, cfg.Harness)
		if err != nil {
			return err
		}
		resolution.AccountID = account
	}
	if strict {
		if m.agentSettings == nil {
			return fmt.Errorf("%w: preflight_unavailable", ports.ErrAgentSettingsCapabilitiesUnavailable)
		}
		settings, err := m.agentSettings.ValidateSpawn(ctx, id, resolution.Settings, clearEffort)
		if err != nil {
			return err
		}
		resolution.EffortCleared = settings.Effort != resolution.Settings.Effort
		if resolution.EffortCleared {
			resolution.Sources.Effort = "provider"
		}
		resolution.Settings = settings
		resolution.Validation = "catalog"
		resolution.Reason = ""
		if settings.Model == "" && settings.Effort == "" && settings.FastMode == domain.AgentFastDefault {
			resolution.Validation = "provider-default"
		}
	}
	cfg.AgentConfig.Model = string(resolution.Settings.Model)
	cfg.AgentConfig.Effort = string(resolution.Settings.Effort)
	if strict {
		cfg.AgentConfig.FastMode = resolution.Settings.FastMode
	}
	cfg.AgentConfig.Launch.FastMode = resolution.Settings.FastMode == domain.AgentFastOn
	cfg.AgentConfig.Launch.FastModeOff = resolution.Settings.FastMode == domain.AgentFastOff
	cfg.AgentConfigResolved = true
	cfg.SpawnAgentSettings = resolution
	if store, ok := m.store.(spawnAgentSettingsStore); ok {
		return store.SaveSpawnAgentSettings(ctx, id, *resolution)
	}
	if strict {
		return fmt.Errorf("%w: spawn_storage_unavailable", ports.ErrAgentSettingsCapabilitiesUnavailable)
	}
	return nil
}
