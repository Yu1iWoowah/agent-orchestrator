package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type switchSettingsRuntime struct {
	svc      *chatsvc.Service
	timedOut atomic.Bool
}

func (r *switchSettingsRuntime) QueueAgentSettings(ctx context.Context, op domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	// Bound the existing non-reentrant gate failure without waiting for the
	// production switch's 45-second recovery deadline.
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	result, err := r.svc.QueueAgentSettings(ctx, op)
	if errors.Is(err, context.DeadlineExceeded) {
		r.timedOut.Store(true)
	}
	return result, err
}

func (r *switchSettingsRuntime) CancelAgentSettings(ctx context.Context, id domain.SessionID, opID string, version int64) (domain.AgentSettingsOperation, error) {
	return r.svc.CancelAgentSettings(ctx, id, opID, version)
}

type switchSettingsConversation struct{ *journalConversation }

func (c *switchSettingsConversation) ListConfigOptions(ctx context.Context) ([]ports.ChatConfigOption, error) {
	return (liveModelConversation{c.fakeConversation}).ListConfigOptions(ctx)
}

func TestProviderSwitchWithSettingsControlDoesNotReenterLifecycleGate(t *testing.T) {
	for _, choice := range []string{"fast", "live_model"} {
		t.Run(choice, func(t *testing.T) {
			h, _, driver := newEditHarness(t, false)
			for _, provider := range []domain.AgentAccountProvider{domain.AgentAccountProviderCodex, domain.AgentAccountProviderClaude} {
				if err := h.st.BindSessionAgentAccount(t.Context(), testSession, provider, domain.DeviceAgentAccountID(provider), h.now()); err != nil {
					t.Fatal(err)
				}
			}
			driver.target = func(base *fakeConversation) ports.ChatConversation {
				conv := &switchSettingsConversation{&journalConversation{fakeConversation: base}}
				conv.quiet.Store(true)
				return conv
			}
			catalog := ports.AgentModelCatalog{AgentID: "claude-code", Models: []ports.AgentModelInfo{{ID: "opus", IsDefault: true, SupportsFast: true}}}
			raw, _ := json.Marshal(catalog)
			if err := h.st.UpsertAgentModelCatalog(t.Context(), ports.CachedAgentModelCatalog{AgentID: "claude-code", CatalogJSON: string(raw), LastSuccessAt: h.now(), FetchedAt: h.now(), InputFingerprint: "live-inputs"}); err != nil {
				t.Fatal(err)
			}
			reader := agentsettings.New(h.st, h.now)
			reader.SetCatalogFingerprintSource(legacyCatalogInputs{})
			runtime := &switchSettingsRuntime{svc: h.svc}
			control := agentsettings.NewControl(reader, h.st, agentsettings.NewAuthorizer(h.st, nil, h.now), runtime, h.now)
			h.svc.SetAgentSettingsMutator(control)
			h.svc.SetAgentSettingsValidator(control)
			request := domain.ProviderSwitchRequest{TargetHarness: domain.HarnessClaudeCode, FastMode: choice == "fast"}
			if choice == "live_model" {
				request.Model = "retired-pick"
			}
			done := make(chan error, 1)
			go func() { done <- h.svc.SwitchProvider(t.Context(), testSession, request) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("provider switch blocked on its own lifecycle gate")
			}
			if runtime.timedOut.Load() {
				t.Fatal("provider switch re-entered its lifecycle gate: context deadline exceeded")
			}
			saved, err := h.st.SessionAgentSettings(t.Context(), testSession)
			if err != nil || choice == "fast" && saved.Settings.FastMode != domain.AgentFastOn || choice == "live_model" && saved.Settings.Model != "opus" {
				t.Fatalf("switch silently dropped selection: %+v %v", saved, err)
			}
		})
	}
}
