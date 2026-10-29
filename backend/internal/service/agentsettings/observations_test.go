package agentsettings

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type observationSource struct {
	receipt domain.AgentSettingsObservation
}

func (s observationSource) ObserveAgentSettings(context.Context, domain.SessionID) (domain.AgentSettingsObservation, error) {
	return s.receipt, nil
}

func TestSettingsReadExplicitlyReportsUnknownEffective(t *testing.T) {
	store := catalogEvidenceStore{settings: domain.SessionAgentSettings{SessionID: "s", Harness: domain.HarnessClaudeCode, Settings: domain.AgentSettings{FastMode: domain.AgentFastOn}}}
	got, err := New(store, time.Now).Read(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	var wire struct {
		Effective struct {
			FastMode domain.AgentSettingObservation `json:"fastMode"`
		} `json:"effective"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Effective.FastMode.State != domain.AgentSettingUnknown || wire.Effective.FastMode.Value != "" {
		t.Fatalf("missing evidence masquerades as effective: %s", raw)
	}
}

func TestSettingsReadFencesProviderReceipts(t *testing.T) {
	now := time.Now().UTC()
	observed := now.Add(-time.Minute)
	settings := domain.SessionAgentSettings{SessionID: "s", Harness: domain.HarnessClaudeCode, AccountID: "device-claude", OwnerGeneration: "g", ProviderConversationID: "native", BranchID: "branch", Settings: domain.AgentSettings{FastMode: domain.AgentFastOn}}
	owner := domain.AgentSettingsOwner{SessionID: "s", AccountID: "device-claude", ControllerGeneration: "g", ProviderConversationID: "native", BranchID: "branch"}
	for _, tc := range []struct {
		name   string
		change func(*domain.AgentSettingsObservation)
		want   domain.AgentSettingState
	}{
		{"not offered", func(*domain.AgentSettingsObservation) {}, domain.AgentSettingNotProvided},
		{"other account", func(o *domain.AgentSettingsObservation) { o.Owner.AccountID = "managed" }, domain.AgentSettingUnknown},
		{"other generation", func(o *domain.AgentSettingsObservation) { o.Owner.ControllerGeneration = "replacement" }, domain.AgentSettingUnknown},
		{"other provider branch", func(o *domain.AgentSettingsObservation) { o.Owner.ProviderConversationID = "fork" }, domain.AgentSettingUnknown},
		{"other AO branch", func(o *domain.AgentSettingsObservation) { o.Owner.BranchID = "fork" }, domain.AgentSettingUnknown},
		{"future", func(o *domain.AgentSettingsObservation) {
			future := now.Add(time.Second)
			o.Effective.ObservedAt = &future
		}, domain.AgentSettingUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt := domain.AgentSettingsObservation{Owner: owner, Effective: domain.AgentSettingsEffective{FastMode: domain.AgentSettingObservation{State: domain.AgentSettingNotProvided, Reason: "provider_not_offered"}, ObservedAt: &observed, Source: "provider_config"}}
			tc.change(&receipt)
			svc := New(catalogEvidenceStore{settings: settings}, func() time.Time { return now })
			setter, ok := any(svc).(interface {
				SetObservationSource(ports.AgentSettingsObservationSource)
			})
			if !ok {
				t.Fatal("live provider receipt source is unavailable")
			}
			setter.SetObservationSource(observationSource{receipt})
			got, err := svc.Read(t.Context(), "s")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(got)
			var wire struct {
				Effective domain.AgentSettingsEffective `json:"effective"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Effective.FastMode.State != tc.want || wire.Effective.FastMode.Value != "" {
				t.Fatalf("unqualified effective receipt: %s", raw)
			}
			if got.Settings.FastMode != domain.AgentFastOn {
				t.Fatal("observation overwrote requested fast")
			}
		})
	}
}
