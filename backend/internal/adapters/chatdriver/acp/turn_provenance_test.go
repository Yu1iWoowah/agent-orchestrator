package acp

import (
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestACPTurnBoundariesIdentifyNativeOwner(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, compact := range []bool{false, true} {
			name := "new"
			if resume {
				name = "resumed"
			}
			if compact {
				name += "/compact"
			}
			t.Run(name, func(t *testing.T) {
				agent := &fakeAgent{promptNoPermission: true, capabilities: &acpsdk.AgentCapabilities{
					SessionCapabilities: acpsdk.SessionCapabilities{Resume: &acpsdk.SessionResumeCapabilities{}},
				}}
				driver := forkTestDriver(agent)
				driver.cfg.Capabilities[ports.ChatCapabilityCompaction] = true
				cfg := ports.ChatStartConfig{SessionID: "source", WorkspacePath: t.TempDir()}
				var conv ports.ChatConversation
				var err error
				if resume {
					conv, err = driver.Resume(t.Context(), ports.ChatResumeConfig{
						SessionID: cfg.SessionID, WorkspacePath: cfg.WorkspacePath, ProviderConversationID: "owned:existing-native",
					})
				} else {
					conv, err = driver.Start(t.Context(), cfg)
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conv.Close() })
				if compact {
					_, err = conv.(ports.ChatCompactor).Compact(t.Context())
				} else {
					var ref ports.ChatTurnRef
					ref, err = conv.SendTurn(t.Context(), ports.ChatUserMessage{Text: "prepare this handoff"})
					if err == nil {
						err = conv.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				var started string
				for {
					event := nextEvent(t, conv.Events())
					if event.Kind != ports.ChatEventTurnStarted && event.Kind != ports.ChatEventTurnCompleted {
						continue
					}
					if event.ProviderConversationID != conv.ProviderConversationID() {
						t.Errorf("%s owner = %q, want %q", event.Kind, event.ProviderConversationID, conv.ProviderConversationID())
					}
					if event.Kind == ports.ChatEventTurnStarted {
						started = event.ProviderTurnID
					} else {
						if started == "" || event.ProviderTurnID != started || event.TurnState != domain.TurnStateCompleted {
							t.Fatalf("unsettled or mismatched boundaries: %q -> %+v", started, event)
						}
						return
					}
				}
			})
		}
	}
}
