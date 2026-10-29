package chat_test

import (
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestBackgroundSettingsSnapshotUsesOwnLaunchAndLowestEffortWithoutRootRevision(t *testing.T) {
	conversation := &backgroundModelConversation{backgroundTestConversation: &backgroundTestConversation{terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()}}, models: []ports.ChatModel{{ID: "background-model", Default: true, Efforts: []string{"high", "low"}}}}
	var started ports.ChatStartConfig
	svc := chatsvc.New(chatsvc.Options{Drivers: fakeRegistry{driver: fakeDriver{conv: conversation, startCfg: &started}}, NewID: func() string { return "background-id" }})
	if _, err := svc.RunBackgroundTask(t.Context(), domain.HarnessCodex, ports.ChatStartConfig{Model: "background-model", Effort: "high", FastMode: domain.AgentFastOff}, "unchanged input"); err != nil {
		t.Fatal(err)
	}
	messages := conversation.sentMessages()
	if len(messages) != 1 {
		t.Fatal("background turn lost")
	}
	raw, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Frame *domain.AgentSettingsTurnSnapshot `json:"SettingsSnapshot"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	frame := wire.Frame
	if frame == nil || frame.Source != "background" || frame.Revision != nil || frame.Owner == nil || frame.Owner.SessionID != started.SessionID || frame.Owner.ControllerGeneration != started.ProviderScopeID || frame.Owner.ProviderConversationID != conversation.ProviderConversationID() || frame.Settings == nil || frame.Settings.Model != "background-model" || frame.Settings.Effort != "low" || frame.Settings.FastMode != domain.AgentFastOff {
		t.Fatalf("background borrowed root/unknown choices: %s", raw)
	}
	if messages[0].Text != "unchanged input" || messages[0].Settings.Model != "" || messages[0].Settings.Effort != "low" {
		t.Fatalf("background launch/wire semantics changed: %+v", messages[0])
	}
}
