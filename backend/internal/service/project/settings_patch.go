package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// PatchSettingsInput carries only edited leaves and their previously observed
// values. Objects merge recursively; arrays replace; null removes a key.
type PatchSettingsInput struct {
	DisplayName *string        `json:"displayName,omitempty"`
	Config      map[string]any `json:"config,omitempty"`
	Expected    map[string]any `json:"expected"`
}

type settingsConflict struct {
	Path    string `json:"path"`
	Current any    `json:"current"`
}

// PatchSettings compares and writes under the same admission gate as spawn.
func (m *Service) PatchSettings(ctx context.Context, id domain.ProjectID, in PatchSettingsInput) (Project, error) {
	if err := validateProjectID(id); err != nil {
		return Project{}, err
	}
	for key := range in.Config {
		switch key {
		case "defaultBranch", "sessionPrefix", "agentAccounts", "worker", "orchestrator", "reviewers", "autoReview", "workersRequestReview", "trackerIntake", "env", "postCreate", "preRemove":
		default:
			return Project{}, apierr.Invalid("INVALID_PROJECT_CONFIG", "Setting cannot be patched: "+key, nil)
		}
	}
	var row domain.ProjectRecord
	write := func(locked context.Context) error {
		var ok bool
		var err error
		row, ok, err = m.store.MutateProjectSettings(locked, string(id), func(current domain.ProjectRecord) (domain.ProjectRecord, error) {
			return mergeProjectSettings(locked, current, in)
		})
		if err != nil {
			var apiError *apierr.Error
			if errors.As(err, &apiError) {
				return apiError
			}
			return apierr.Internal("PROJECT_SETTINGS_UPDATE_FAILED", "Failed to update project settings")
		}
		if !ok {
			return apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
		}
		return nil
	}
	var err error
	if m.withProjectGate != nil {
		err = m.withProjectGate(ctx, id, write)
	} else {
		err = write(ctx)
	}
	if err != nil {
		return Project{}, err
	}
	return m.projectFromRow(ctx, row), nil
}

func mergeProjectSettings(ctx context.Context, row domain.ProjectRecord, in PatchSettingsInput) (domain.ProjectRecord, error) {
	visible := row.Config
	if ports.IsRemoteAccountRequest(ctx) {
		visible = visible.WithoutLocalAccounts()
	}
	encoded, err := json.Marshal(visible)
	if err != nil {
		return row, err
	}
	var config map[string]any
	if err := json.Unmarshal(encoded, &config); err != nil {
		return row, err
	}
	// GET omits a wholly unset config, including its empty role objects.
	if row.Config.IsZero() {
		clear(config)
	}
	current := map[string]any{"displayName": projectDisplayName(row), "config": config}
	patch := map[string]any{}
	if in.DisplayName != nil {
		patch["displayName"] = *in.DisplayName
	}
	if in.Config != nil {
		patch["config"] = in.Config
	}
	if in.Expected == nil {
		return row, apierr.Invalid("PROJECT_SETTINGS_EXPECTED_REQUIRED", "Expected values are required", nil)
	}
	var conflicts []settingsConflict
	if err := compareSettingsLeaves(current, patch, in.Expected, "", &conflicts); err != nil {
		return row, err
	}
	if len(conflicts) > 0 {
		return row, apierr.Conflict("PROJECT_SETTINGS_CONFLICT", "Project settings changed", map[string]any{"conflicts": conflicts})
	}
	mergeSettingsObject(config, in.Config)
	migrateLegacyAgentConfig(config, in.Config)
	encoded, err = json.Marshal(config)
	if err != nil {
		return row, err
	}
	var proposed domain.ProjectConfig
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposed); err != nil {
		return row, apierr.Invalid("INVALID_PROJECT_CONFIG", err.Error(), nil)
	}
	proposed, err = accountConfigForMutation(ctx, row.Config, proposed)
	if err != nil {
		return row, err
	}
	if err := proposed.Validate(); err != nil {
		return row, apierr.Invalid("INVALID_PROJECT_CONFIG", err.Error(), nil)
	}
	if row.Kind.WithDefault() == domain.ProjectKindScratch {
		if err := validateScratchProjectConfig(proposed); err != nil {
			return row, apierr.Invalid("INVALID_PROJECT_CONFIG", err.Error(), nil)
		}
	}
	if err := proposed.ValidateCanonicalRepository(row.RepoOriginURL); err != nil {
		return row, apierr.Invalid("INVALID_PROJECT_CONFIG", err.Error(), nil)
	}
	if in.DisplayName != nil {
		name := strings.TrimSpace(*in.DisplayName)
		if name == "" {
			return row, apierr.Invalid("DISPLAY_NAME_REQUIRED", "Display name is required", nil)
		}
		if utf8.RuneCountInString(name) > maxDisplayNameLen && name != strings.TrimSpace(projectDisplayName(row)) {
			return row, apierr.Invalid("DISPLAY_NAME_TOO_LONG", fmt.Sprintf("Display name must be %d characters or fewer", maxDisplayNameLen), nil)
		}
		row.DisplayName = name
	}
	row.Config = proposed
	return row, nil
}

func compareSettingsLeaves(current, patch, expected map[string]any, prefix string, conflicts *[]settingsConflict) error {
	keys := make([]string, 0, len(patch))
	for key := range patch {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if object, ok := patch[key].(map[string]any); ok {
			currentObject, _ := current[key].(map[string]any)
			expectedObject, _ := expected[key].(map[string]any)
			if err := compareSettingsLeaves(currentObject, object, expectedObject, path, conflicts); err != nil {
				return err
			}
			continue
		}
		value, ok := expected[key]
		if !ok {
			return apierr.Invalid("PROJECT_SETTINGS_EXPECTED_REQUIRED", "Missing expected value for "+path, nil)
		}
		if !reflect.DeepEqual(current[key], value) {
			*conflicts = append(*conflicts, settingsConflict{Path: path, Current: current[key]})
		}
	}
	return nil
}

func mergeSettingsObject(current, patch map[string]any) {
	for key, value := range patch {
		if value == nil {
			delete(current, key)
		} else if object, ok := value.(map[string]any); ok {
			target, ok := current[key].(map[string]any)
			if !ok {
				target = map[string]any{}
				current[key] = target
			}
			mergeSettingsObject(target, object)
		} else {
			current[key] = value
		}
	}
}

func migrateLegacyAgentConfig(config, patch map[string]any) {
	legacy, _ := config["agentConfig"].(map[string]any)
	roles := []map[string]any{}
	rolePatches := []map[string]any{}
	touched := map[string]bool{}
	for _, key := range []string{"worker", "orchestrator"} {
		role, _ := config[key].(map[string]any)
		if role == nil {
			role = map[string]any{}
			config[key] = role
		}
		roles = append(roles, role)
		rolePatch, _ := patch[key].(map[string]any)
		agentPatch, _ := rolePatch["agentConfig"].(map[string]any)
		rolePatches = append(rolePatches, agentPatch)
		for field := range agentPatch {
			touched[field] = true
		}
	}
	reviewers, _ := config["reviewers"].([]any)
	for _, reviewer := range reviewers {
		if role, ok := reviewer.(map[string]any); ok {
			roles = append(roles, role)
			var agentPatch map[string]any
			if _, replaced := patch["reviewers"]; replaced {
				agentPatch, _ = role["agentConfig"].(map[string]any)
			}
			rolePatches = append(rolePatches, agentPatch)
		}
	}
	if reviewersPatch, ok := patch["reviewers"].([]any); ok {
		for _, reviewer := range reviewersPatch {
			role, _ := reviewer.(map[string]any)
			agent, _ := role["agentConfig"].(map[string]any)
			for field := range agent {
				touched[field] = true
			}
		}
	}
	for _, field := range []string{"model", "mode", "effort", "permissions"} {
		value, exists := legacy[field]
		if !exists || !touched[field] {
			continue
		}
		for i, role := range roles {
			if _, edited := rolePatches[i][field]; edited {
				continue
			}
			agent, _ := role["agentConfig"].(map[string]any)
			if agent == nil {
				agent = map[string]any{}
				role["agentConfig"] = agent
			}
			if explicit, ok := agent[field]; !ok || explicit == "" {
				agent[field] = value
			}
		}
		delete(legacy, field)
	}
}
