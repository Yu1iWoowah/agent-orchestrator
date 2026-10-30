package agentsettings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type settingsHistoryEvidenceStore struct {
	catalogEvidenceStore
	operations []domain.AgentSettingsOperation
}

func (s settingsHistoryEvidenceStore) ListAgentSettingsOperations(context.Context, domain.SessionID, int) ([]domain.AgentSettingsOperation, error) {
	return s.operations, nil
}

func TestSettingsHistoryPreservesActorAndUnresolvedWriterEvidence(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		retained := domain.AgentSettingsOperation{
			ID: "retained", SessionID: "s", Version: 4, Phase: domain.AgentSettingsCancelled, ErrorCode: "AGENT_SETTINGS_SESSION_TERMINATED",
			Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal},
			TUI:   &domain.AgentSettingsTUIHandoff{UnconfirmedRuntime: !stopped, TerminationVerified: stopped, SourceRuntimeHandle: "private-handle", TargetGeneration: "private-generation"},
		}
		store := settingsHistoryEvidenceStore{
			catalogEvidenceStore: catalogEvidenceStore{settings: domain.SessionAgentSettings{SessionID: "s", Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, IsTerminated: true, Settings: domain.AgentSettings{FastMode: domain.AgentFastOff}}},
			operations:           []domain.AgentSettingsOperation{retained, {ID: "applied", Phase: domain.AgentSettingsApplied, Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorOrchestrator, SessionID: "verified-orchestrator"}, Target: domain.AgentSettings{FastMode: domain.AgentFastOn}}},
		}
		snapshot, err := New(store, time.Now).Read(t.Context(), "s")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			History []struct {
				domain.AgentSettingsOperation
				WriterStatus string `json:"writerStatus"`
			} `json:"history"`
			WriterRecovery *struct {
				OperationID string `json:"operationId"`
				Version     int64  `json:"version"`
			} `json:"writerRecovery"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if len(wire.History) != 2 || wire.History[1].Actor.SessionID != "verified-orchestrator" || wire.History[1].Actor.Kind != domain.AgentSettingsActorOrchestrator {
			t.Fatalf("durable verified actor/history disappeared: %s", raw)
		}
		if stopped {
			if wire.WriterRecovery != nil || wire.History[0].WriterStatus != "stopped" {
				t.Fatalf("confirmed stop still shown as unresolved: %s", raw)
			}
		} else if wire.WriterRecovery == nil || wire.WriterRecovery.OperationID != "retained" || wire.WriterRecovery.Version != 4 || wire.History[0].WriterStatus != "unconfirmed" {
			t.Fatalf("terminated unresolved writer has no actionable evidence: %s", raw)
		}
		if strings.Contains(string(raw), "private-handle") || strings.Contains(string(raw), "private-generation") || snapshot.Settings.FastMode != domain.AgentFastOff || snapshot.Effective.FastMode.State != domain.AgentSettingUnknown {
			t.Fatalf("history leaked ownership or projected old requested values as current/effective: %s", raw)
		}
	}
}
