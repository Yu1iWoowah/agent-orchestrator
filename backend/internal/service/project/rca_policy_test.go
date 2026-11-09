package project_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestRCAPolicyDurableAndIndependent(t *testing.T) {
	st, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertProject(t.Context(), domain.ProjectRecord{ID: "agent-orchestrator", Path: t.TempDir(), DisplayName: "project"}); err != nil {
		t.Fatal(err)
	}
	on, off := true, false
	rcaCalls, reportingCalls := 0, 0
	notify := func(id domain.ProjectID, snapshot domain.RCAPolicySnapshot) {
		rcaCalls++
		p, ok, err := st.GetProject(t.Context(), string(id))
		if err != nil || !ok || p.Config.RCA.Enabled == nil || snapshot.Project == nil || *p.Config.RCA.Enabled != *snapshot.Project {
			t.Fatal("RCA notified before durable policy")
		}
	}
	svc := project.NewWithDeps(project.Deps{Store: st, RCAPolicyChanged: notify, ReportingPolicyChanged: func(domain.ProjectID, domain.HCGReportingPolicySnapshot) { reportingCalls++ }})
	config := domain.ProjectConfig{RCA: domain.RCAConfig{Enabled: &on}}
	if _, err := svc.SetConfig(t.Context(), "agent-orchestrator", project.SetConfigInput{Config: config}); err != nil {
		t.Fatal(err)
	}
	if rcaCalls != 1 || reportingCalls != 0 {
		t.Fatal("RCA changed reporting")
	}
	config.HCGReporting.Enabled = &on
	if _, err := svc.SetConfig(t.Context(), "agent-orchestrator", project.SetConfigInput{Config: config}); err != nil {
		t.Fatal(err)
	}
	if rcaCalls != 1 || reportingCalls != 1 {
		t.Fatal("reporting changed RCA")
	}
	config.Worker.RCA.Enabled = &off
	if _, err := svc.UpdateSettings(t.Context(), "agent-orchestrator", project.UpdateSettingsInput{DisplayName: "project", Config: config}); err != nil {
		t.Fatal(err)
	}
	if rcaCalls != 2 || reportingCalls != 1 {
		t.Fatal("worker policy not independent")
	}
	if _, err := svc.PatchSettings(t.Context(), "agent-orchestrator", project.PatchSettingsInput{Config: map[string]any{"worker": map[string]any{"rca": map[string]any{"enabled": true}}}, Expected: map[string]any{"config": map[string]any{"worker": map[string]any{"rca": map[string]any{"enabled": false}}}}}); err != nil {
		t.Fatal(err)
	}
	if rcaCalls != 3 {
		t.Fatal("patch missed cancellation")
	}
	failing := project.NewWithDeps(project.Deps{Store: failingProjectUpsertStore{st}, RCAPolicyChanged: notify})
	if _, err := failing.SetConfig(t.Context(), "agent-orchestrator", project.SetConfigInput{}); err == nil {
		t.Fatal("expected failure")
	}
	if rcaCalls != 3 {
		t.Fatal("failed write revoked policy")
	}
}
