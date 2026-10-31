package acp

import (
	"log/slog"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSettlementKeepsItemsStableWhileProviderUpdates(t *testing.T) {
	c := &conversation{
		log:    slog.New(slog.DiscardHandler),
		events: make(chan ports.ChatEvent),
		messages: map[string]string{
			"message-1": "first", "message-2": "second", "message-3": "third",
		},
		thoughts:       map[string]string{"thought": "original thought"},
		nestedMessages: map[string]nestedMessageState{"nested": {text: "original nested", parentID: "parent"}},
		tools: map[string]*toolState{
			"tool": {id: "tool", title: "original tool", status: acpsdk.ToolCallStatusInProgress},
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.settleOpenItems("turn")
	}()

	// Receiving the first item proves settlement has taken its snapshot. The
	// unbuffered channel holds it at the next item while provider state changes.
	first := nextEvent(t, c.events)
	if first.ProviderItemID != "message-1" {
		t.Fatalf("first settled item = %q", first.ProviderItemID)
	}
	c.mu.Lock()
	c.messages["message-3"] = "late message"
	c.thoughts["thought"] = "late thought"
	c.nestedMessages["nested"] = nestedMessageState{text: "late nested", parentID: "parent"}
	c.mu.Unlock()
	title := "late tool"
	c.mergeToolUpdate(&acpsdk.SessionToolCallUpdate{ToolCallId: "tool", Title: &title})

	items := map[string]ports.ChatEvent{first.ProviderItemID: first}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-c.events:
			items[event.ProviderItemID] = event
		case <-done:
			for id, want := range map[string]string{
				"message-1": "first", "message-2": "second", "message-3": "third",
				"thought": "original thought", "nested": "original nested",
			} {
				if got := items[id].Text; got != want {
					t.Errorf("settled %s = %q, want %q", id, got, want)
				}
			}
			if got := items["tool"].Summary; got != "original tool" {
				t.Errorf("settled tool summary = %q, want original tool", got)
			}
			return
		case <-deadline:
			t.Fatal("settlement did not finish")
		}
	}
}
