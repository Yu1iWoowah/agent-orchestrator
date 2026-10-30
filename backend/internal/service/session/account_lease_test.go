package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAccountLeaseConflictsRemainRetryableOnSpawnAndResume(t *testing.T) {
	for _, category := range []struct {
		err  error
		code string
	}{
		{ports.ErrAgentAccountInUse, "AGENT_ACCOUNT_IN_USE"},
		{ports.ErrAgentAccountOperationBusy, "AGENT_ACCOUNT_OPERATION_BUSY"},
	} {
		for _, operation := range []string{"spawn", "resume"} {
			t.Run(operation+"/"+category.code, func(t *testing.T) {
				st := newFakeStore()
				st.projects["mer"] = domain.ProjectRecord{ID: "mer"}
				cause := fmt.Errorf("private-provider-canary: %w", category.err)
				manager := &fakeCommander{spawnErr: cause, restoreErr: cause}
				svc := NewWithDeps(Deps{Store: st, Manager: manager})
				var err error
				if operation == "spawn" {
					_, _, _, err = svc.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode})
				} else {
					_, err = svc.ResumeAgent(context.Background(), "mer-1")
				}
				var got *apierr.Error
				if !errors.As(err, &got) || got.Kind != apierr.KindConflict || got.Code != category.code || got.Details["retryable"] != true {
					t.Fatalf("lease rejection lost retryable conflict: %#v %v", got, err)
				}
				if strings.Contains(got.Message, "canary") || strings.Contains(got.Message, "cleanup is scheduled") {
					t.Fatalf("incorrect lease message: %s", got.Message)
				}
			})
		}
	}
}
