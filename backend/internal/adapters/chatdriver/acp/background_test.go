package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestClaudeBackgroundCycleHasDurableTurnBoundaries(t *testing.T) {
	c := &conversation{
		claudeBackgroundTurns: true,
		sessionID:             "native", log: slog.New(slog.DiscardHandler),
		events:   make(chan ports.ChatEvent, 32),
		messages: make(map[string]string), thoughts: make(map[string]string),
		nestedMessages: make(map[string]nestedMessageState), tools: make(map[string]*toolState),
	}
	// Claude's pinned bridge exposes these SDK frames through its opt-in
	// _claude/sdkMessage extension, before the corresponding ACP output.
	c.handleNotification("_claude/sdkMessage", json.RawMessage(`{"sessionId":"native","message":{"type":"system","subtype":"session_state_changed","state":"running","uuid":"cycle-1"}}`))
	var update acpsdk.SessionNotification
	if err := json.Unmarshal([]byte(`{"sessionId":"native","update":{"sessionUpdate":"agent_message_chunk","messageId":"answer","content":{"type":"text","text":"Background finished"}}}`), &update); err != nil {
		t.Fatal(err)
	}
	if err := c.SessionUpdate(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	c.handleNotification("_claude/sdkMessage", json.RawMessage(`{"sessionId":"native","message":{"type":"result","subtype":"success","uuid":"result-1","origin":{"kind":"task-notification"}}}`))
	var started, completed string
	for len(c.events) > 0 {
		event := <-c.events
		switch event.Kind {
		case ports.ChatEventTurnStarted:
			started = event.ProviderTurnID
		case ports.ChatEventMessageDelta:
			if started == "" || event.ProviderTurnID != started {
				t.Fatalf("background output has no preceding correlated turn: %+v", event)
			}
		case ports.ChatEventTurnCompleted:
			completed = event.ProviderTurnID
		}
	}
	if started == "" || completed != started {
		t.Fatalf("background cycle boundaries = %q -> %q", started, completed)
	}
}

func TestClaudeBackgroundReconnectSkipsPreviousCommittedPrompt(t *testing.T) {
	initialize, _ := json.Marshal(acpsdk.InitializeResponse{ProtocolVersion: acpsdk.ProtocolVersionNumber})
	session, _ := json.Marshal(acpsdk.NewSessionResponse{SessionId: "native"})
	daemon, host := net.Pipe()
	t.Cleanup(func() { _ = host.Close() })
	driver := New(Config{Harness: domain.HarnessClaudeCode, ClaudeBackgroundTurns: true, Capabilities: ports.ChatCapabilities{ports.ChatCapabilityResume: true}, Launch: func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil }}, slog.New(slog.DiscardHandler))
	driver.connectHost = func(context.Context, persistenthost.Config) (*persistenthost.Transport, error) {
		return &persistenthost.Transport{Stdin: daemon, Stdout: daemon, Reconnected: true, ACPState: &persistenthost.ACPState{InitializeResult: initialize, SessionResult: session, SessionID: "native", PendingResultEventID: "old-result", RetainUnpromptedEvents: true}}, nil
	}
	opened, err := driver.Resume(context.Background(), ports.ChatResumeConfig{SessionID: "ao", DataDir: t.TempDir(), WorkspacePath: t.TempDir(), ProviderConversationID: "native"})
	if err != nil {
		t.Fatal(err)
	}
	c := opened.(*conversation)
	t.Cleanup(func() { _ = c.Close() })
	ack := make(chan string, 1)
	go func() { frame, _ := bufio.NewReader(host).ReadBytes('\n'); ack <- string(frame) }()
	go func() {
		for _, frame := range []string{
			`{"method":"session/update","params":{"sessionId":"native","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"OLD-PROMPT"}}}}`,
			`{"method":"_ao/persistent_prompt_result","params":{"eventId":"old-result","result":{"stopReason":"end_turn"}}}`,
			`{"method":"_claude/sdkMessage","params":{"sessionId":"native","_meta":{"ao.unprompted":"true"},"message":{"type":"system","subtype":"session_state_changed","state":"running","uuid":"cycle"}}}`,
			`{"method":"session/update","params":{"sessionId":"native","_meta":{"ao.persistentEventId":"new-output"},"update":{"sessionUpdate":"agent_message_chunk","messageId":"new-answer","content":{"type":"text","text":"BACKGROUND-REPLAY"}}}}`,
			`{"method":"_claude/sdkMessage","params":{"sessionId":"native","_meta":{"ao.unprompted":"true","ao.persistentEventId":"new-result"},"message":{"type":"result","subtype":"success"}}}`,
		} {
			if _, err := fmt.Fprintln(host, frame); err != nil {
				return
			}
		}
	}()
	if err := c.ActivateLiveReconnect(context.Background(), "claude-background:cycle"); err != nil {
		t.Fatal(err)
	}
	for {
		event := nextEvent(t, c.events)
		if event.Kind == ports.ChatEventMessageDelta && event.Delta != "BACKGROUND-REPLAY" {
			t.Fatalf("previous prompt replay leaked into background turn: %+v", event)
		}
		if event.Kind == ports.ChatEventTurnCompleted {
			if event.ProviderEventID != "new-result" || event.ProviderTurnID != "claude-background:cycle" {
				t.Fatalf("previous prompt result completed background turn: %+v", event)
			}
			break
		}
	}
	select {
	case frame := <-ack:
		if !strings.Contains(frame, "old-result") {
			t.Fatalf("wrong terminal receipt ACK: %s", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("previous committed prompt was not ACKed")
	}
}

func TestClaudeBackgroundLifecycleLeavesAOPromptAlone(t *testing.T) {
	c := &conversation{sessionID: "native", activeTurn: "ao-prompt", claudeBackgroundTurns: true, log: slog.New(slog.DiscardHandler), events: make(chan ports.ChatEvent, 8)}
	for _, frame := range []string{
		`{"sessionId":"native","message":{"type":"system","subtype":"session_state_changed","state":"running","uuid":"normal"}}`,
		`{"sessionId":"native","message":{"type":"result","subtype":"success","uuid":"normal-result"}}`,
	} {
		if err := c.claudeBackgroundEvent(json.RawMessage(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if c.activeTurn != "ao-prompt" || len(c.events) != 0 {
		t.Fatalf("raw lifecycle changed the AO prompt: active=%s events=%d", c.activeTurn, len(c.events))
	}
}

func TestClaudeBackgroundIdleWithoutResultReleasesController(t *testing.T) {
	c := &conversation{sessionID: "native", activeTurn: "claude-background:cycle", backgroundTurn: "claude-background:cycle", events: make(chan ports.ChatEvent, 8)}
	if err := c.claudeBackgroundEvent(json.RawMessage(`{"sessionId":"native","message":{"type":"system","subtype":"session_state_changed","state":"idle","uuid":"idle"}}`)); err != nil {
		t.Fatal(err)
	}
	completed := nextEvent(t, c.events)
	if completed.Kind != ports.ChatEventTurnCompleted || completed.TurnState != domain.TurnStateFailed || completed.Err == nil {
		t.Fatalf("missing-result cycle was not failed: %+v", completed)
	}
	ready := nextEvent(t, c.events)
	if ready.ControllerState != ports.ChatControllerReady || c.activeTurn != "" {
		t.Fatalf("background cycle left controller busy: %+v", ready)
	}
}

func TestClaudeBackgroundWaitsForPreviousPromptSettlement(t *testing.T) {
	c := &conversation{sessionID: "native", activeTurn: "previous", promptFinished: make(chan struct{}), events: make(chan ports.ChatEvent, 8)}
	finished := c.promptFinished
	done := make(chan error, 1)
	go func() {
		done <- c.claudeBackgroundEvent(json.RawMessage(`{"sessionId":"native","_meta":{"ao.unprompted":"true"},"message":{"type":"system","subtype":"session_state_changed","state":"running","uuid":"next"}}`))
	}()
	c.mu.Lock()
	c.activeTurn = ""
	close(finished)
	c.promptFinished = nil
	c.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("background lifecycle did not follow the previous settlement")
	}
	started := nextEvent(t, c.events)
	if started.ProviderTurnID != "claude-background:next" || !started.ProviderInitiated {
		t.Fatalf("background cycle has wrong correlation: %+v", started)
	}
}

func TestClaudeBackgroundSettlementWaitStopsOnDetach(t *testing.T) {
	closing := make(chan struct{})
	close(closing)
	c := &conversation{sessionID: "native", activeTurn: "previous", promptFinished: make(chan struct{}), closing: closing, events: make(chan ports.ChatEvent, 8)}
	done := make(chan error, 1)
	go func() {
		done <- c.claudeBackgroundEvent(json.RawMessage(`{"sessionId":"native","_meta":{"ao.unprompted":"true"},"message":{"type":"system","subtype":"session_state_changed","state":"running","uuid":"next"}}`))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("background notification remained blocked after the controller detached")
	}
	if len(c.events) != 0 {
		t.Fatal("detached controller emitted a new background turn")
	}
}
