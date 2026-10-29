package chat_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type recordingSettingsMutator struct {
	mu      sync.Mutex
	patches []domain.AgentSettingsPatch
}

type legacyCatalogInputs struct{}

func (legacyCatalogInputs) CatalogFingerprint(context.Context, string, string) (string, error) {
	return "live-inputs", nil
}

func TestLegacySettingsAndTypedControlShareRevisionAndFrozenTurn(t *testing.T) {
	for _, harness := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		t.Run(string(harness), func(t *testing.T) { legacySettingsAndTypedControl(t, harness) })
	}
}

func legacySettingsAndTypedControl(t *testing.T, agentHarness domain.AgentHarness) {
	for _, entry := range []string{"turn_settings", "config_options", "direct_settings", "effort", "fast_on", "fast_off", "fast_off_after_on"} {
		t.Run(entry, func(t *testing.T) {
			h, conv, _, _ := journalHarnessForHarness(t, agentHarness)
			catalog := ports.AgentModelCatalog{AgentID: string(agentHarness), Models: []ports.AgentModelInfo{{ID: "new-model", IsDefault: true, Efforts: []string{"high"}, SupportsFast: true}}}
			raw, _ := json.Marshal(catalog)
			if err := h.st.UpsertAgentModelCatalog(t.Context(), ports.CachedAgentModelCatalog{AgentID: string(agentHarness), CatalogJSON: string(raw), InputFingerprint: "live-inputs", LastSuccessAt: h.now(), FetchedAt: h.now()}); err != nil {
				t.Fatal(err)
			}
			reader := agentsettings.New(h.st, h.now)
			reader.SetCatalogFingerprintSource(legacyCatalogInputs{})
			control := agentsettings.NewControl(reader, h.st, agentsettings.NewAuthorizer(h.st, nil, h.now), h.svc, h.now)
			h.svc.SetAgentSettingsMutator(control)
			h.svc.SetAgentSettingsValidator(control)
			if entry == "fast_off_after_on" {
				on := true
				if _, err := h.svc.SetConfigOption(t.Context(), testSession, "fast", ports.ChatConfigOptionValue{Boolean: &on}); err != nil {
					t.Fatal(err)
				}
			}
			stepsBefore := conv.stepCount()

			first, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "first"})
			if err != nil {
				t.Fatal(err)
			}
			saved, err := h.st.TurnByID(t.Context(), first.ID)
			if err != nil || saved.AgentSettings == nil {
				t.Fatal(err)
			}
			original := saved.AgentSettings.Clone()
			picked := h.ctrl.Settings()
			picked.Model = "new-model"
			switch entry {
			case "turn_settings":
				_, err = h.svc.SetTurnSettings(t.Context(), testSession, picked)
			case "config_options":
				_, err = h.svc.SetConfigOption(t.Context(), testSession, "model", ports.ChatConfigOptionValue{Select: "new-model"})
			case "effort":
				_, err = h.svc.SetConfigOption(t.Context(), testSession, "effort", ports.ChatConfigOptionValue{Select: "high"})
			case "fast_on", "fast_off", "fast_off_after_on":
				on := entry == "fast_on"
				_, err = h.svc.SetConfigOption(t.Context(), testSession, "fast", ports.ChatConfigOptionValue{Boolean: &on})
			default:
				err = h.ctrl.SetSettings(t.Context(), picked)
			}
			if err != nil {
				t.Fatal(err)
			}
			pending, found, err := h.st.PendingAgentSettingsOperation(t.Context(), testSession)
			if err != nil || !found || conv.stepCount() != stepsBefore {
				t.Fatalf("busy bypass %+v %v", pending, err)
			}
			if _, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "next"}); err != nil {
				t.Fatal(err)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: first.ProviderTurnID, TurnState: domain.TurnStateCompleted})
			h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
				op, e := h.st.AgentSettingsOperation(t.Context(), testSession, pending.ID)
				return e == nil && op.Phase == domain.AgentSettingsApplied && len(conv.sentMessages()) == 2
			})
			wire := conv.sentMessages()[1].Settings
			if wire.Model != string(pending.Target.Model) || wire.Effort != string(pending.Target.Effort) || wire.FastMode != pending.Target.FastMode || wire.Fast != (pending.Target.FastMode == domain.AgentFastOn) || conv.legacySetters.Load() != 0 {
				t.Fatalf("next wire %+v", wire)
			}
			saved, err = h.st.TurnByID(t.Context(), first.ID)
			if err != nil || saved.AgentSettings == nil || *saved.AgentSettings.Settings != *original.Settings || *saved.AgentSettings.Revision != *original.Revision {
				t.Fatal("current turn changed after pending acceptance")
			}
			durable, err := h.st.ConversationForSession(t.Context(), testSession)
			durableFast := durable.Settings.AgentFastMode
			if durableFast == "" {
				durableFast = domain.AgentFastDefault
			}
			if err != nil || durableFast != pending.Target.FastMode || durable.Settings.FastMode != (pending.Target.FastMode == domain.AgentFastOn) {
				t.Fatalf("durable fast diverged: %+v %v", durable.Settings, err)
			}
			current, err := h.st.SessionAgentSettings(t.Context(), testSession)
			if err != nil || current.Revision != pending.BaseRevision+1 || current.Settings != pending.Target {
				t.Fatalf("divergent revision %+v %v", current, err)
			}
		})
	}
}

func (m *recordingSettingsMutator) ChangeAgentSettings(_ context.Context, _ domain.SessionID, p domain.AgentSettingsPatch) (domain.AgentSettingsOperation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.patches = append(m.patches, p)
	return domain.AgentSettingsOperation{Phase: domain.AgentSettingsPending}, nil
}

func TestLegacySettingsEntrancesUseSchedulerWithoutNativeSetters(t *testing.T) {
	h, conv, _, _ := journalHarness(t)
	m := &recordingSettingsMutator{}
	installer, ok := any(h.svc).(interface {
		SetAgentSettingsMutator(ports.ChatAgentSettingsMutator)
	})
	if !ok {
		t.Fatal("legacy settings have no shared mutation boundary")
	}
	installer.SetAgentSettingsMutator(m)
	if _, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "busy"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetConfigOption(t.Context(), testSession, "model", ports.ChatConfigOptionValue{Select: "next"}); err != nil {
		t.Fatal(err)
	}
	picked := h.ctrl.Settings()
	picked.Model = "another"
	if _, err := h.svc.SetTurnSettings(t.Context(), testSession, picked); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.patches) != 2 || m.patches[0].Model == nil || *m.patches[0].Model != "next" || m.patches[1].Model == nil || *m.patches[1].Model != "another" {
		t.Fatalf("legacy patches: %+v", m.patches)
	}
	if conv.legacySetters.Load() != 0 || conv.stepCount() != 0 || h.ctrl.Settings().Model == "another" {
		t.Fatal("busy legacy entrance bypassed scheduler")
	}
}
