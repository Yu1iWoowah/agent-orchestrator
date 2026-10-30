package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestWorkerClaudeAliasRequiresModelIDEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models []ports.AgentModelInfo
		want   error
	}{
		{"label only", []ports.AgentModelInfo{{ID: "custom", Label: "Claude Opus 5.5", Efforts: []string{"xhigh"}}}, ports.ErrModelCapabilitiesUnavailable},
		{"newest no efforts", []ports.AgentModelInfo{{ID: "claude-opus-4-1", Efforts: []string{"xhigh"}}, {ID: "claude-opus-5-5"}}, ports.ErrUnsupportedEffort},
		{"ID overrides label", []ports.AgentModelInfo{{ID: "claude-opus-4-1", Label: "Opus 9.9", Efforts: []string{"low"}}, {ID: "claude-opus-5-5", Label: "Sonnet 1.0", Efforts: []string{"xhigh"}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &Manager{modelCatalog: tuningCatalog{catalog: ports.AgentModelCatalog{Models: append([]ports.AgentModelInfo{{ID: "opus", IsDefault: true}}, tc.models...)}}}
			got, err := manager.resolveAgentConfig(context.Background(), ports.SpawnConfig{Harness: domain.HarnessClaudeCode, Kind: domain.KindWorker, AgentConfig: ports.AgentConfig{Model: "opus", Effort: "xhigh"}, EffortOverride: true}, domain.ProjectConfig{})
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("got %+v err=%v want %v", got, err, tc.want)
				}
				return
			}
			if err != nil || got.Model != "opus" || got.Effort != "xhigh" {
				t.Fatalf("got %+v err=%v", got, err)
			}
		})
	}
}

func TestWorkerUnknownAliasClearsOnlyInheritedEffortAfterModelChange(t *testing.T) {
	for _, modelID := range []string{"us.anthropic.claude-opus-4-1-20250805-v1:0", "claude-opus-4-1@20250805", "gateway/opus-current"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%t", modelID, explicit), func(t *testing.T) {
				manager := &Manager{modelCatalog: tuningCatalog{catalog: ports.AgentModelCatalog{Models: []ports.AgentModelInfo{{ID: "opus", IsDefault: true}, {ID: modelID, Label: "Claude Opus 4.1"}}}}}
				project := domain.ProjectConfig{Worker: domain.RoleOverride{AgentConfig: domain.AgentConfig{Effort: "high"}}}
				cfg := ports.SpawnConfig{Harness: domain.HarnessClaudeCode, Kind: domain.KindWorker, AgentConfig: ports.AgentConfig{Model: "opus"}, EffortOverride: explicit}
				if explicit {
					cfg.AgentConfig.Effort = "high"
				}
				got, err := manager.resolveAgentConfig(context.Background(), cfg, project)
				if explicit {
					if !errors.Is(err, ports.ErrModelCapabilitiesUnavailable) {
						t.Fatalf("explicit unknown effort accepted: config=%+v err=%v", got, err)
					}
					return
				}
				if err != nil || got.Model != "opus" || got.Effort != "" {
					t.Fatalf("inherited unknown effort was not cleared: config=%+v err=%v", got, err)
				}
			})
		}
	}
}
