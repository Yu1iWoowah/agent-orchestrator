package project_test

import (
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestSettingsAgentCannotRaiseProjectPolicyButLocalCan(t *testing.T) {
	st := sqlitetest.MustOpen(t)
	ctx := t.Context()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "policy", Path: "/fake", RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	svc := project.NewWithDeps(project.Deps{Store: st})
	policy := &domain.AgentSettingsPolicy{Enabled: true, Harnesses: map[domain.AgentHarness]domain.AgentSettingsAllowance{domain.HarnessCodex: {Models: []domain.AgentModelSelection{"native"}, Efforts: []domain.AgentEffortSelection{""}}}}
	in := project.SetConfigInput{Config: domain.ProjectConfig{AgentSettingsPolicy: policy}}
	agentCtx := ports.WithAgentSettingsCaller(ctx, ports.AgentSettingsCaller{SessionID: "orch", Grant: "fake"})
	if _, err := svc.SetConfig(agentCtx, "policy", in); err == nil {
		t.Fatal("agent raised its own settings authority")
	} else {
		var e *apierr.Error
		if !errors.As(err, &e) || e.Code != "AGENT_SETTINGS_POLICY_DENIED" {
			t.Fatal(err)
		}
	}
	if _, err := svc.SetConfig(ctx, "policy", in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetConfig(agentCtx, "policy", in); err != nil {
		t.Fatal("unchanged policy should preserve existing config operations:", err)
	}
	if _, err := svc.SetConfig(agentCtx, "policy", project.SetConfigInput{}); err == nil {
		t.Fatal("agent removed its policy")
	}
}
