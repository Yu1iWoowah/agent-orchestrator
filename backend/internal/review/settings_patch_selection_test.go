package review

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

func TestSettingsPatchPreservesConfiguredReviewerSelection(t *testing.T) {
	for _, edit := range []struct {
		field string
		value string
	}{
		{"model", "worker-model"}, {"mode", "low"},
		{"effort", "low"}, {"permissions", "accept-edits"},
	} {
		for _, editedRole := range []string{"worker", "orchestrator"} {
			for _, ownConfig := range []bool{false, true} {
				name := "empty"
				if ownConfig {
					name = "own"
				}
				t.Run(edit.field+"/"+editedRole+"/"+name, func(t *testing.T) {
					ctx := t.Context()
					st := newSQLiteReviewStore(t)
					cfg := domain.ProjectConfig{
						AgentConfig: domain.AgentConfig{Model: "claude-only", Mode: "high", Effort: "high", Permissions: domain.PermissionModeAuto},
						Worker:      domain.RoleOverride{Harness: domain.HarnessClaudeCode},
						Reviewers:   []domain.ReviewerConfig{{Harness: domain.ReviewerCodex}},
					}
					if ownConfig {
						cfg.Reviewers[0].AgentConfig = domain.AgentConfig{Model: "review-model", Mode: "low", Effort: "low", Permissions: domain.PermissionModeAcceptEdits}
					}
					if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), Config: cfg}); err != nil {
						t.Fatal(err)
					}
					engine := &Engine{projects: st}
					worker := domain.SessionRecord{ProjectID: "p"}
					beforeHarness, before, err := engine.projectReviewerSelection(ctx, worker)
					if err != nil || beforeHarness != domain.ReviewerCodex || before != cfg.Reviewers[0].AgentConfig {
						t.Fatalf("configured reviewer baseline = %s %+v, %v", beforeHarness, before, err)
					}
					_, err = project.New(st).PatchSettings(ctx, "p", project.PatchSettingsInput{
						Config:   map[string]any{editedRole: map[string]any{"agentConfig": map[string]any{edit.field: edit.value}}},
						Expected: map[string]any{"config": map[string]any{editedRole: map[string]any{"agentConfig": map[string]any{edit.field: nil}}}},
					})
					if err != nil {
						t.Fatal(err)
					}
					afterHarness, after, err := engine.projectReviewerSelection(ctx, worker)
					if err != nil || afterHarness != beforeHarness || after != before {
						t.Fatalf("configured reviewer selection changed: before=%s %+v after=%s %+v err=%v", beforeHarness, before, afterHarness, after, err)
					}
				})
			}
		}
	}
}
