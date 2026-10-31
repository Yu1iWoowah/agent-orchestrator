package modelcapabilities

import (
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestClaudeAliasEffortEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected ports.AgentModelInfo
		models   []ports.AgentModelInfo
		efforts  []string
		known    bool
	}{
		{name: "alias case preserves capability lookup", selected: ports.AgentModelInfo{ID: "Opus[1m]"}, models: []ports.AgentModelInfo{{ID: "claude-opus-5-5", Efforts: []string{"xhigh"}}}, efforts: []string{"xhigh"}, known: true},
		{name: "explicit alias evidence", selected: ports.AgentModelInfo{ID: "opus", Efforts: []string{"low"}}, models: []ports.AgentModelInfo{{ID: "claude-opus-5-5", Efforts: []string{"xhigh"}}}, efforts: []string{"low"}, known: true},
		{name: "numeric versions and snapshot dates", selected: ports.AgentModelInfo{ID: "opus[1m]"}, models: []ports.AgentModelInfo{{ID: "claude-opus-5-9-20261031", Efforts: []string{"high"}}, {ID: "claude-opus-5-10-20260101", Efforts: []string{"xhigh"}}}, efforts: []string{"xhigh"}, known: true},
		{name: "duplicate latest agrees", selected: ports.AgentModelInfo{ID: "opus"}, models: []ports.AgentModelInfo{{ID: "claude-opus-5-5", Efforts: []string{"high"}}, {ID: "claude-opus-5-5-20261031", Efforts: []string{"high"}}}, efforts: []string{"high"}, known: true},
		{name: "duplicate latest conflicts", selected: ports.AgentModelInfo{ID: "opus"}, models: []ports.AgentModelInfo{{ID: "claude-opus-5-5", Efforts: []string{"high"}}, {ID: "claude-opus-5-5-20261031", Efforts: []string{"xhigh"}}}},
		{name: "latest empty is not skipped", selected: ports.AgentModelInfo{ID: "opus"}, models: []ports.AgentModelInfo{{ID: "claude-opus-4-1", Efforts: []string{"xhigh"}}, {ID: "claude-opus-5-5"}}, known: true},
		{name: "different provider", selected: ports.AgentModelInfo{ID: "opus", Provider: "anthropic"}, models: []ports.AgentModelInfo{{ID: "claude-opus-5-5", Provider: "other", Efforts: []string{"xhigh"}}}},
		{name: "label only", selected: ports.AgentModelInfo{ID: "opus"}, models: []ports.AgentModelInfo{{ID: "custom", Label: "Claude Opus 5.5", Efforts: []string{"xhigh"}}}},
		{name: "concrete empty is known", selected: ports.AgentModelInfo{ID: "claude-opus-5-5"}, known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := Efforts(domain.HarnessClaudeCode, tc.models, tc.selected)
			if known != tc.known || !slices.Equal(got, tc.efforts) {
				t.Fatalf("efforts=%v known=%v, want %v/%v", got, known, tc.efforts, tc.known)
			}
		})
	}
}
