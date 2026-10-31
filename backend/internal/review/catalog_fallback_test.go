package review

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type lastKnownGoodReviewerModels struct {
	catalog   ports.AgentModelCatalog
	refreshes []bool
}

func (m *lastKnownGoodReviewerModels) ModelCatalog(_ context.Context, _, _ string, refresh bool) (ports.AgentModelCatalog, error) {
	m.refreshes = append(m.refreshes, refresh)
	catalog := m.catalog
	catalog.Stale = true
	if refresh {
		catalog.RefreshState = "error"
		catalog.RefreshError = "credential_expired"
	}
	return catalog, nil
}

func TestReviewerLastKnownGoodCatalog(t *testing.T) {
	for _, tc := range []struct {
		harness       domain.ReviewerHarness
		model, effort string
		models        []ports.AgentModelInfo
	}{
		{domain.ReviewerClaudeCode, "", "xhigh", []ports.AgentModelInfo{{ID: "opus", IsDefault: true}, {ID: "claude-opus-5-5", Efforts: []string{"xhigh"}}}},
		{domain.ReviewerCodex, "native-model", "high", []ports.AgentModelInfo{{ID: "native-model", IsDefault: true, Efforts: []string{"high"}}}},
	} {
		for _, supported := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/supported=%t", tc.harness, supported), func(t *testing.T) {
				effort := tc.effort
				if !supported {
					effort = "low"
				}
				config := domain.AgentConfig{Model: tc.model, Effort: effort}
				worker := liveWorker()
				launcher := &fakeLauncher{handle: "review-handle"}
				engine := newEngineForTest(&fakeStore{}, fakeSessions{rec: worker, ok: true}, prAt("sha1"), fakeProjects{cfg: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: tc.harness, AgentConfig: config}}}}, launcher)
				models := &lastKnownGoodReviewerModels{catalog: ports.AgentModelCatalog{Models: tc.models}}
				engine.models = models
				_, err := engine.TriggerWithOptions(context.Background(), worker.ID, TriggerOptions{})
				if !slices.Equal(models.refreshes, []bool{false, true}) {
					t.Fatalf("expected cached lookup then refresh: %v", models.refreshes)
				}
				if !supported {
					if !errors.Is(err, ports.ErrAgentSettingsUnsupported) || launcher.spawned {
						t.Fatalf("cached effort validation: err=%v spawned=%v", err, launcher.spawned)
					}
					return
				}
				if err != nil || !launcher.spawned || launcher.gotSpec.AgentConfig.Model != config.Model || launcher.gotSpec.AgentConfig.Effort != config.Effort {
					t.Fatalf("last-known-good launch: err=%v spawned=%v config=%+v", err, launcher.spawned, launcher.gotSpec.AgentConfig)
				}
			})
		}
	}
}
