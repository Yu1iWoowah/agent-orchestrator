package agentsettings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type reconciliationEvidenceStore struct {
	catalogEvidenceStore
	attempt domain.AgentSettingsReconciliation
}

func (s reconciliationEvidenceStore) ListAgentSettingsReconciliations(context.Context, domain.SessionID) ([]domain.AgentSettingsReconciliation, error) {
	return []domain.AgentSettingsReconciliation{s.attempt}, nil
}

func (s reconciliationEvidenceStore) PendingChatDeliveries(context.Context, domain.SessionID) ([]domain.ChatDeliveryAttempt, error) {
	return nil, nil
}

func TestReconciliationReadPreservesFailureWithoutExposingExecutorOrFreshness(t *testing.T) {
	at := time.Now().UTC()
	st := reconciliationEvidenceStore{
		catalogEvidenceStore: catalogEvidenceStore{settings: domain.SessionAgentSettings{SessionID: "s", Harness: domain.HarnessCodex, Settings: domain.AgentSettings{FastMode: domain.AgentFastOff}}},
		attempt:              domain.AgentSettingsReconciliation{ID: "attempt-1", SessionID: "s", Phase: "blocked", FailedStep: "observing", ErrorCode: "AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE", SafeCause: "capabilities_unavailable", RequestID: "request-1", ExecutorID: "private-executor-sentinel", Desired: domain.AgentSettings{Model: "desired-model", FastMode: domain.AgentFastOff}, ObservationAvailability: "unknown", CreatedAt: at, LastProgressAt: at, RecoveryActions: []string{"retry"}},
	}
	snapshot, err := New(st, time.Now).Read(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"attempt-1", "observing", "AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE", "request-1", "desired-model", `"recoveryActions":["retry"]`} {
		if !strings.Contains(string(raw), expected) {
			t.Fatalf("missing diagnostic %q: %s", expected, raw)
		}
	}
	if strings.Contains(string(raw), "private-executor-sentinel") || snapshot.Effective.Model.State != domain.AgentSettingUnknown || snapshot.Reconciliations[0].Observed != nil {
		t.Fatalf("claim leaked or desired became observed: %s", raw)
	}
}
