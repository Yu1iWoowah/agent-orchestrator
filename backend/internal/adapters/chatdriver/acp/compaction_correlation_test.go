package acp

import (
	"context"
	"errors"
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

func TestCompactionPrechecksProveNotSent(t *testing.T) {
	for _, reason := range []string{"cancelled", "closed", "unsupported", "busy", "no-session"} {
		t.Run(reason, func(t *testing.T) {
			c := &conversation{sessionID: "session", capabilities: ports.ChatCapabilities{ports.ChatCapabilityCompaction: true}}
			ctx := t.Context()
			switch reason {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "closed":
				c.closed = true
			case "unsupported":
				c.capabilities = nil
			case "busy":
				c.activeTurn = "other"
			case "no-session":
				c.sessionID = ""
			}
			_, err := c.Compact(ports.WithChatCompactionID(ctx, "not-forwarded"))
			if !errors.Is(err, ports.ErrChatCompactionNotSent) {
				t.Fatalf("pre-I/O refusal retained unknown: %v", err)
			}
			if c.compactingTurnID != "" {
				t.Fatalf("unsent command reserved: %s", c.compactingTurnID)
			}
		})
	}
}
