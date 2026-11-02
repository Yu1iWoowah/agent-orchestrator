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

func TestCompactionOutcomeRequiresNativeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name            string
		reason          acpsdk.StopReason
		err             *acpsdk.RequestError
		responseFailure bool
		want            ports.ChatCompactionOutcome
	}{
		{name: "native end", reason: acpsdk.StopReasonEndTurn, want: ports.ChatCompactionSettled},
		{name: "native cancelled", reason: acpsdk.StopReasonCancelled, want: ports.ChatCompactionSettled},
		{name: "native token limit", reason: acpsdk.StopReasonMaxTokens, want: ports.ChatCompactionSettled},
		{name: "native request limit", reason: acpsdk.StopReasonMaxTurnRequests, want: ports.ChatCompactionSettled},
		{name: "native refusal", reason: acpsdk.StopReasonRefusal, want: ports.ChatCompactionSettled},
		{name: "native failed result", reason: acpsdk.StopReasonEndTurn, responseFailure: true, want: ports.ChatCompactionSettled},
		{name: "missing stop reason", want: ports.ChatCompactionUnknown},
		{name: "future stop reason", reason: "future", want: ports.ChatCompactionUnknown},
		{name: "ordinary RPC error", err: &acpsdk.RequestError{Code: -32603, Message: "outcome unknown"}, want: ports.ChatCompactionUnknown},
		{name: "validation RPC error", err: &acpsdk.RequestError{Code: -32602, Message: "invalid params"}, want: ports.ChatCompactionUnknown},
		{name: "unsupported method", err: &acpsdk.RequestError{Code: -32601, Message: "method not found"}, want: ports.ChatCompactionNotSent},
		{name: "unsupported variant", err: &acpsdk.RequestError{Code: -32600, Message: "unknown variant `session/prompt`"}, want: ports.ChatCompactionNotSent},
		{name: "wrong variant", err: &acpsdk.RequestError{Code: -32600, Message: "unknown variant `session/load`"}, want: ports.ChatCompactionUnknown},
		{name: "wrong code", err: &acpsdk.RequestError{Code: -32603, Message: "unknown variant `session/prompt`"}, want: ports.ChatCompactionUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &conversation{sessionID: "native", activeTurn: "command", compactingTurnID: "command", events: make(chan ports.ChatEvent, 16), log: slog.New(slog.DiscardHandler)}
			response := acpsdk.PromptResponse{StopReason: tc.reason}
			var promptErr error
			if tc.err != nil {
				tc.err.Data = map[string]any{persistenthost.ACPEventIDMetaKey: "host-result"}
				promptErr = tc.err
			} else {
				response.Meta = map[string]any{persistenthost.ACPEventIDMetaKey: "host-result"}
			}
			if tc.responseFailure {
				c.promptResponseFailure = func(acpsdk.PromptResponse) error { return errors.New("native operation failed") }
			}
			c.finishPrompt("command", response, promptErr)
			var terminal *ports.ChatEvent
			for len(c.events) > 0 {
				event := <-c.events
				if event.Kind == ports.ChatEventCompacted && tc.want == ports.ChatCompactionUnknown {
					t.Fatal("unknown reply projected a completed compaction")
				}
				if event.Kind == ports.ChatEventTurnCompleted {
					terminal = &event
				}
			}
			if terminal == nil || terminal.Compaction == nil || terminal.Compaction.CommandID != "command" || terminal.Compaction.Outcome != tc.want {
				t.Fatalf("missing or incorrect execution evidence: %+v", terminal)
			}
			if tc.want == ports.ChatCompactionUnknown {
				if terminal.CompactionID != "" || c.compactingTurnID != "command" {
					t.Fatal("unknown reply erased the original command or claimed settlement")
				}
				if _, err := c.SendTurn(t.Context(), ports.ChatUserMessage{Text: "next"}); !errors.Is(err, ports.ErrChatTurnNotSentBusy) {
					t.Fatalf("unknown compact allowed another native prompt: %v", err)
				}
			} else if terminal.CompactionID != "command" || c.compactingTurnID != "" {
				t.Fatal("qualified terminal/refusal did not settle the command")
			}
			if tc.responseFailure && terminal.Err == nil {
				t.Fatal("test did not exercise a failed native terminal")
			}
		})
	}
}
