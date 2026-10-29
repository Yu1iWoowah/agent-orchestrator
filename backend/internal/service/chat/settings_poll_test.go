package chat_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type pendingPickerConversation struct{ *journalConversation }

func (*pendingPickerConversation) ListConfigOptions(context.Context) ([]ports.ChatConfigOption, error) {
	return []ports.ChatConfigOption{{ID: "model", Category: "model", Type: ports.ChatConfigOptionSelect, Current: ports.ChatConfigOptionValue{Select: "opus"}, Choices: []ports.ChatConfigOptionChoice{{Value: "opus", Name: "Opus"}, {Value: "sonnet", Name: "Sonnet"}}}}, nil
}

func TestBusyPickerHTTPPollKeepsPendingSelection(t *testing.T) {
	for _, harness := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		t.Run(string(harness), func(t *testing.T) {
			conv := &journalConversation{fakeConversation: newFakeConversation()}
			conv.quiet.Store(true)
			provider := &pendingPickerConversation{conv}
			accountProvider, _ := domain.AgentAccountProviderFor(harness)
			h := newHarnessWithConversationAndStoreForHarness(t, provider, func(st *sqlite.Store) chatsvc.Store {
				if err := st.BindSessionAgentAccount(t.Context(), testSession, accountProvider, domain.DeviceAgentAccountID(accountProvider), time.Now()); err != nil {
					t.Fatal(err)
				}
				return st
			}, harness)
			rec, _, err := h.st.GetSession(t.Context(), testSession)
			if err != nil {
				t.Fatal(err)
			}
			rec.Metadata.ProviderConversationID = conv.ProviderConversationID()
			if err := h.st.UpdateSession(t.Context(), rec); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(ports.AgentModelCatalog{AgentID: string(harness), Models: []ports.AgentModelInfo{{ID: "opus", IsDefault: true}, {ID: "sonnet"}}})
			if err := h.st.UpsertAgentModelCatalog(t.Context(), ports.CachedAgentModelCatalog{AgentID: string(harness), CatalogJSON: string(raw), InputFingerprint: "live-inputs", LastSuccessAt: h.now(), FetchedAt: h.now()}); err != nil {
				t.Fatal(err)
			}
			core := agentsettings.New(h.st, h.now)
			core.SetCatalogFingerprintSource(legacyCatalogInputs{})
			control := agentsettings.NewControl(core, h.st, agentsettings.NewAuthorizer(h.st, nil, h.now), h.svc, h.now)
			h.svc.SetAgentSettingsMutator(control)
			h.svc.SetAgentSettingsValidator(control)
			if _, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "busy"}); err != nil {
				t.Fatal(err)
			}
			wireModel := conv.sentMessages()[0].Settings.Model
			patched, err := h.svc.SetConfigOption(t.Context(), testSession, "model", ports.ChatConfigOptionValue{Select: "sonnet"})
			if err != nil || len(patched) == 0 || patched[0].Current.Select != "sonnet" {
				t.Fatalf("mutation did not project the accepted choice: %+v %v", patched, err)
			}
			router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.DiscardHandler), nil, httpd.APIDeps{Conversations: h.svc}, httpd.ControlDeps{})
			// Exercise the same GET and currentValue field used by the picker,
			// both immediately and at its next five-second polling boundary.
			for _, delay := range []time.Duration{0, 5 * time.Second} {
				time.Sleep(delay)
				request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+string(testSession)+"/conversation/config-options", nil)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				var payload struct {
					Options []struct {
						ID           string `json:"id"`
						CurrentValue any    `json:"currentValue"`
					} `json:"options"`
				}
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &payload) != nil {
					t.Fatalf("picker poll failed: %d %s", response.Code, response.Body.String())
				}
				selected := ""
				for _, option := range payload.Options {
					if option.ID == "model" {
						selected, _ = option.CurrentValue.(string)
					}
				}
				if selected != "sonnet" {
					t.Errorf("picker poll after %s reverted pending sonnet to %q", delay, selected)
				}
				current, err := h.st.SessionAgentSettings(t.Context(), testSession)
				live, liveErr := provider.ListConfigOptions(t.Context())
				if err != nil || liveErr != nil || current.Revision != 0 || current.Settings.Model != "" || h.ctrl.Settings().Model != "" || live[0].Current.Select != "opus" || conv.stepCount() != 0 || conv.sentMessages()[0].Settings.Model != wireModel {
					t.Fatalf("menu projection changed provider, committed settings or current wire: %+v %v %v", current, err, liveErr)
				}
			}
		})
	}
}
