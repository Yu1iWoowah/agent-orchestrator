package project_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestDelegationPolicyNotificationAfterDurableSettings(t *testing.T) {
	st, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertProject(t.Context(), domain.ProjectRecord{ID: "p", Path: t.TempDir(), DisplayName: "project"}); err != nil {
		t.Fatal(err)
	}
	on, off := true, false
	calls := 0
	notify := func(id domain.ProjectID, _ domain.HCGDelegationPolicySnapshot) {
		calls++
		p, ok, err := st.GetProject(t.Context(), string(id))
		if err != nil || !ok || (p.Config.HCGDelegation.Enabled == nil && p.Config.Worker.HCGDelegation.Enabled == nil) {
			t.Fatalf("notification preceded durable config: %+v %v", p, err)
		}
	}
	svc := project.NewWithDeps(project.Deps{Store: st, DelegationPolicyChanged: notify})
	ref := domain.HCGProfileRef{ID: "openai-fixture", Revision: "v1"}
	config := domain.ProjectConfig{HCGDelegation: domain.HCGDelegationConfig{Enabled: &on, AllowedProfiles: []domain.HCGProfileRef{ref}, SelectedProfile: &ref}}
	if _, err := svc.SetConfig(t.Context(), "p", project.SetConfigInput{Config: config}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("set-config notification: %d", calls)
	}
	if _, err := svc.UpdateSettings(t.Context(), "p", project.UpdateSettingsInput{DisplayName: "renamed", Config: config}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unrelated settings cancelled reports")
	}
	config.Worker.HCGDelegation.Enabled = &off
	if _, err := svc.UpdateSettings(t.Context(), "p", project.UpdateSettingsInput{DisplayName: "renamed", Config: config}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("role change notification: %d", calls)
	}
	if _, err := svc.PatchSettings(t.Context(), "p", project.PatchSettingsInput{Config: map[string]any{"worker": map[string]any{"hcgDelegation": map[string]any{"enabled": true}}}, Expected: map[string]any{"config": map[string]any{"worker": map[string]any{"hcgDelegation": map[string]any{"enabled": false}}}}}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("role patch notification: %d", calls)
	}
	failing := project.NewWithDeps(project.Deps{Store: failingProjectUpsertStore{st}, DelegationPolicyChanged: notify})
	if _, err := failing.SetConfig(t.Context(), "p", project.SetConfigInput{}); err == nil {
		t.Fatal("expected store failure")
	}
	if calls != 3 {
		t.Fatal("failed write notified cancellation")
	}
}
