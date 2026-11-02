package acp

import (
	"log/slog"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCompactionCompletionRequiresNativeReceiptAndOriginalTurn(t *testing.T) {
	for _, tc := range []struct{ name, compacting, eventID, want string }{
		{"original", "command", "host-result", "command"},
		{"background", "other-command", "host-result", ""},
		{"without-native-receipt", "command", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &conversation{sessionID: "native", activeTurn: "command", compactingTurnID: tc.compacting, events: make(chan ports.ChatEvent, 16), log: slog.New(slog.DiscardHandler)}
			c.finishPrompt("command", acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn, Meta: map[string]any{persistenthost.ACPEventIDMetaKey: tc.eventID}}, nil)
			for len(c.events) > 0 {
				event := <-c.events
				if event.Kind == ports.ChatEventTurnCompleted {
					if event.CompactionID != tc.want {
						t.Fatalf("completion correlation: %+v want %q", event, tc.want)
					}
					return
				}
			}
			t.Fatal("missing native completion")
		})
	}
}
func TestRecoveredCompactionRetainsCorrelationAfterNativeResult(t *testing.T) {
	c := &conversation{sessionID: "native", proc: &process{reconnected: true, gate: newGatedReader(strings.NewReader(""), false)}, liveState: &persistenthost.ACPState{PendingResultEventID: "host-result"}, events: make(chan ports.ChatEvent, 16), log: slog.New(slog.DiscardHandler)}
	ctx := ports.WithChatCompactionID(t.Context(), "command")
	if err := c.ActivateLiveReconnect(ctx, "command"); err != nil {
		t.Fatal(err)
	}
	c.finishPrompt("command", acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn, Meta: map[string]any{persistenthost.ACPEventIDMetaKey: "host-result"}}, nil)
	for len(c.events) > 0 {
		event := <-c.events
		if event.Kind == ports.ChatEventTurnCompleted {
			if event.CompactionID != "command" || event.ProviderEventID != "host-result" {
				t.Fatalf("recovery lost original correlation: %+v", event)
			}
			return
		}
	}
	t.Fatal("missing recovered completion")
}
