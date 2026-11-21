package acp

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Claude Code's own running/idle report becomes provider.state, so a cycle the
// agent starts without an ACP prompt still shows as work.
func TestClaudeSDKSessionStateBecomesProviderState(t *testing.T) {
	conv := &conversation{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessionID: "claude-session",
		events:    make(chan ports.ChatEvent, 8),
	}
	notify := func(params string) {
		t.Helper()
		if !conv.handleNotification(claudeSDKMessageMethod, []byte(params)) {
			t.Fatalf("%s was not handled", claudeSDKMessageMethod)
		}
	}

	notify(`{"sessionId":"claude-session","message":{"type":"system","subtype":"session_state_changed","state":"running"},"_meta":{"` +
		persistenthost.ACPEventIDMetaKey + `":"ev-1"}}`)
	notify(`{"sessionId":"claude-session","message":{"type":"system","subtype":"session_state_changed","state":"idle"}}`)
	// Ignored: an approval reports requires_action itself, other sessions and
	// other SDK messages say nothing about this conversation's work.
	notify(`{"sessionId":"claude-session","message":{"type":"system","subtype":"session_state_changed","state":"requires_action"}}`)
	notify(`{"sessionId":"other-session","message":{"type":"system","subtype":"session_state_changed","state":"running"}}`)
	notify(`{"sessionId":"claude-session","message":{"type":"assistant"}}`)

	want := []struct {
		status  domain.ThreadStatus
		eventID string
	}{{domain.ThreadStatusActive, "ev-1:0"}, {domain.ThreadStatusIdle, ""}}
	for _, w := range want {
		select {
		case event := <-conv.events:
			if event.Kind != ports.ChatEventProviderState || event.ThreadState == nil ||
				event.ThreadState.Status != w.status || event.ProviderEventID != w.eventID {
				t.Fatalf("event = %+v (state %+v), want provider.state %s with id %q", event, event.ThreadState, w.status, w.eventID)
			}
		default:
			t.Fatalf("missing provider.state %s", w.status)
		}
	}
	select {
	case event := <-conv.events:
		t.Fatalf("unexpected event %+v", event)
	default:
	}
}

// With background turns on, every SDK state frame feeds both the background
// turn tracker and provider.state. The turn boundary must come first and only
// once, so provider.state never opens or closes a turn of its own.
func TestClaudeSDKStateFollowsBackgroundTurnBoundaries(t *testing.T) {
	conv := &conversation{
		claudeBackgroundTurns: true,
		log:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessionID:             "native", reportedSessionID: "native",
		events:   make(chan ports.ChatEvent, 16),
		messages: make(map[string]string), thoughts: make(map[string]string),
		nestedMessages: make(map[string]nestedMessageState), tools: make(map[string]*toolState),
	}
	for _, frame := range []string{
		`{"sessionId":"native","message":{"type":"system","subtype":"session_state_changed","state":"running","uuid":"cycle-1"}}`,
		`{"sessionId":"native","message":{"type":"result","subtype":"success","uuid":"result-1"}}`,
		`{"sessionId":"native","message":{"type":"system","subtype":"session_state_changed","state":"idle","uuid":"idle-1"}}`,
	} {
		if !conv.handleNotification(claudeSDKMessageMethod, []byte(frame)) {
			t.Fatalf("%s was not handled", frame)
		}
	}
	var got []string
	for len(conv.events) > 0 {
		event := <-conv.events
		label := string(event.Kind)
		switch {
		case event.ThreadState != nil:
			label += ":" + string(event.ThreadState.Status)
		case event.ControllerState != "":
			label += ":" + string(event.ControllerState)
		}
		got = append(got, label)
	}
	want := []string{"turn.started", "controller.state:busy", "provider.state:active", "turn.completed", "controller.state:ready", "provider.state:idle"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}
