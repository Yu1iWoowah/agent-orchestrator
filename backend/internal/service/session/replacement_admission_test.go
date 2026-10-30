package session

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementAdmissionStore struct{ *fakeStore }

func (s *replacementAdmissionStore) GetActiveOrchestratorReplacement(context.Context, domain.ProjectID) (domain.OrchestratorReplacement, bool, error) {
	return domain.OrchestratorReplacement{ID: "active-operation"}, true, nil
}

func TestReplacementAdmissionRefusesLegacyCleanBeforeNoticeAndRetirement(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: domain.ProjectConfig{Orchestrator: domain.RoleOverride{Harness: domain.HarnessClaudeCode}}}
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindOrchestrator}
	commander := &fakeCommander{}
	svc := &Service{manager: commander, store: &replacementAdmissionStore{st}}
	accountResolutions := 0
	svc.SetAccountResolver(accountResolutionFunc(func(context.Context, ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
		accountResolutions++
		return domain.ResolvedAgentAccount{}, errors.New("account resolution must follow replacement admission")
	}))
	_, err := svc.SpawnOrchestrator(context.Background(), "mer", true, "", "", "explicit-account")
	var api *apierr.Error
	if !errors.As(err, &api) || api.Code != "ORCHESTRATOR_REPLACEMENT_IN_PROGRESS" {
		t.Fatalf("legacy clean bypassed active replacement: %v", err)
	}
	if accountResolutions != 0 || commander.spawned || len(commander.retired) != 0 || len(commander.sent) != 0 {
		t.Fatal("legacy clean resolved an account, sent notice or retired before refusal")
	}
}
