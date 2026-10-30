package sessionmanager

import (
	"context"
	"errors"
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
