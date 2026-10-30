package review

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type aliasReviewerModels struct{ catalog ports.AgentModelCatalog }

func (m aliasReviewerModels) ModelCatalog(context.Context, string, string, bool) (ports.AgentModelCatalog, error) {
	return m.catalog, nil
}

func TestReviewerClaudeAliasTrigger(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, model := range []string{"", "opus", "opus[1m]"} {
			for _, effort := range []string{"", "low", "high", "xhigh", "max"} {
				t.Run(fmt.Sprintf("existing=%t/model=%s/effort=%s", existing, model, effort), func(t *testing.T) {
					worker := liveWorker()
					store := &fakeStore{}
					if existing {
						store.review = &domain.Review{ID: "legacy-review", SessionID: worker.ID, Harness: domain.ReviewerClaudeCode}
					}
					launcher := &fakeLauncher{handle: "review-handle"}
					config := domain.AgentConfig{Model: model, Effort: effort}
					engine := newEngineForTest(store, fakeSessions{rec: worker, ok: true}, prAt("sha1"), fakeProjects{cfg: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerClaudeCode, AgentConfig: config}}}}, launcher)
					engine.models = aliasReviewerModels{ports.AgentModelCatalog{Models: []ports.AgentModelInfo{
						{ID: "opus", IsDefault: true}, {ID: "opus[1m]"},
						{ID: "claude-opus-4-1", Efforts: []string{"low"}},
						{ID: "claude-opus-5-5", Efforts: []string{"low", "high", "xhigh", "max"}},
					}}}
					_, err := engine.TriggerWithOptions(context.Background(), worker.ID, TriggerOptions{})
					if err != nil {
						t.Fatalf("alias trigger failed: %v", err)
					}
					if !launcher.spawned || launcher.gotSpec.AgentConfig.Model != model || launcher.gotSpec.AgentConfig.Effort != effort {
						t.Fatalf("alias launch changed: %+v", launcher.gotSpec.AgentConfig)
					}
				})
			}
		}
	}
}
func TestReviewerClaudeAliasEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness domain.ReviewerHarness
		models  []ports.AgentModelInfo
		want    error
	}{
		{"no family evidence", domain.ReviewerClaudeCode, []ports.AgentModelInfo{{ID: "custom", Label: "Claude Opus 5.5", Efforts: []string{"xhigh"}}}, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"different family", domain.ReviewerClaudeCode, []ports.AgentModelInfo{{ID: "claude-sonnet-5-5", Efforts: []string{"xhigh"}}}, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"other provider region", domain.ReviewerClaudeCode, []ports.AgentModelInfo{{ID: "us.anthropic.claude-opus-5-5-v1:0", Efforts: []string{"xhigh"}}}, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"newest has no efforts", domain.ReviewerClaudeCode, []ports.AgentModelInfo{{ID: "claude-opus-4-1", Efforts: []string{"xhigh"}}, {ID: "claude-opus-5-5"}}, ports.ErrAgentSettingsUnsupported},
		{"non Claude", domain.ReviewerCodex, []ports.AgentModelInfo{{ID: "claude-opus-5-5", Efforts: []string{"xhigh"}}}, ports.ErrAgentSettingsUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := ports.AgentModelCatalog{Models: append([]ports.AgentModelInfo{{ID: "opus", IsDefault: true}}, tc.models...)}
			engine := &Engine{models: aliasReviewerModels{catalog}}
			err := engine.validateReviewerModels(context.Background(), tc.harness, domain.AgentConfig{Effort: "xhigh"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReviewerClaudeAliasUsesSavedCatalog(t *testing.T) {
	models := &savedReviewerModels{catalog: ports.AgentModelCatalog{Models: []ports.AgentModelInfo{{ID: "opus", IsDefault: true}, {ID: "claude-opus-5-5", Efforts: []string{"xhigh"}}}}}
	engine := &Engine{models: models}
	if err := engine.validateReviewerModels(context.Background(), domain.ReviewerClaudeCode, domain.AgentConfig{Effort: "xhigh"}); err != nil {
		t.Fatal(err)
	}
	if len(models.refreshes) != 1 || models.refreshes[0] {
		t.Fatalf("alias unnecessarily rediscovered: %v", models.refreshes)
	}
}

func TestReviewerCatalogDiscoveryUnavailable(t *testing.T) {
	engine := &Engine{models: &savedReviewerModels{}}
	err := engine.validateReviewerModels(context.Background(), domain.ReviewerClaudeCode, domain.AgentConfig{Effort: "xhigh"})
	if !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) {
		t.Fatalf("unclassified discovery failure: %v", err)
	}
}
