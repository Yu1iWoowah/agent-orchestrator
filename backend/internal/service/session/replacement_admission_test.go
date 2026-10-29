package session

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

type replacementAdmissionStore struct{ *fakeStore }

func (s *replacementAdmissionStore) GetActiveOrchestratorReplacement(context.Context, domain.ProjectID) (domain.OrchestratorReplacement, bool, error) {
	return domain.OrchestratorReplacement{ID: "active-operation"}, true, nil
}

func TestReplacementAdmissionRefusesLegacyCleanBeforeNoticeAndRetirement(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer"}
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindOrchestrator}
	commander := &fakeCommander{}
	svc := &Service{manager: commander, store: &replacementAdmissionStore{st}}
	_, err := svc.SpawnOrchestrator(context.Background(), "mer", true, "", "")
	var api *apierr.Error
	if !errors.As(err, &api) || api.Code != "ORCHESTRATOR_REPLACEMENT_IN_PROGRESS" {
		t.Fatalf("legacy clean bypassed active replacement: %v", err)
	}
	if commander.spawned || len(commander.retired) != 0 || len(commander.sent) != 0 {
		t.Fatal("legacy clean sent notice or retired before refusal")
	}
}
