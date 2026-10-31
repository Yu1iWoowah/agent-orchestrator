package acp

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCompletionWindowRefusesTurnAsNotSentBusy(t *testing.T) {
	c := &conversation{
		sessionID: "native", activeTurn: "previous",
		events: make(chan ports.ChatEvent), log: slog.New(slog.DiscardHandler),
	}
	done := make(chan struct{})
	go func() {
		c.finishPrompt("previous", acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil)
		close(done)
	}()
	// The unbuffered completion cannot return until we receive it below, so
	// activeTurn still belongs to the finishing prompt throughout SendTurn.
	if event := nextEvent(t, c.events); event.Kind != ports.ChatEventAccountChanged {
		t.Fatalf("expected recovery before completion: %+v", event)
	}
	_, err := c.SendTurn(context.Background(), ports.ChatUserMessage{Text: "preserve queued input"})
	completed := nextEvent(t, c.events)
	ready := nextEvent(t, c.events)
	<-done
	if !errors.Is(err, ports.ErrChatTurnNotSentBusy) {
		t.Fatalf("unsent completion-window input cannot be retried: %v", err)
	}
	if completed.Kind != ports.ChatEventTurnCompleted || ready.ControllerState != ports.ChatControllerReady {
		t.Fatalf("completion ordering changed: %+v %+v", completed, ready)
	}
	if _, err := c.SendTurn(context.Background(), ports.ChatUserMessage{Text: "preserve queued input"}); err != nil {
		t.Fatalf("retry after Ready: %v", err)
	}
}

func TestSendTurnRechecksBusyAfterSettings(t *testing.T) {
	c := &conversation{sessionID: "native"}
	c.validateSettings = func(ports.PermissionMode, ports.ChatTurnSettings) error {
		c.mu.Lock()
		c.activeTurn = "previous"
		c.mu.Unlock()
		return nil
	}
	if _, err := c.SendTurn(context.Background(), ports.ChatUserMessage{Text: "queued input"}); !errors.Is(err, ports.ErrChatTurnNotSentBusy) {
		t.Fatalf("unsent input racing settings cannot be retried: %v", err)
	}
}
