package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const claudeBackgroundTurnPrefix = "claude-background:"

// The bridge emits raw lifecycle frames before the ACP updates they describe.
// Only these two SDK types are requested; notification contents never become a
// user message. The native state UUID survives host replay and identifies the
// unsolicited turn independently of any AO prompt.
func (c *conversation) claudeBackgroundEvent(raw json.RawMessage) error {
	var notification struct {
		SessionID string         `json:"sessionId"`
		Meta      map[string]any `json:"_meta"`
		Message   struct {
			Type       string `json:"type"`
			Subtype    string `json:"subtype"`
			State      string `json:"state"`
			UUID       string `json:"uuid"`
			IsError    bool   `json:"is_error"`
			StopReason string `json:"stop_reason"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &notification); err != nil {
		return err
	}
	c.mu.Lock()
	if notification.SessionID != c.sessionID {
		c.mu.Unlock()
		return fmt.Errorf("claude lifecycle for unexpected session %q", notification.SessionID)
	}
	// A prompt response has already crossed the host before an unprompted frame,
	// but its SDK waiter may not yet have released the prior turn. Wait for that
	// local settlement rather than attributing the new output to the old prompt.
	finished := c.promptFinished
	unprompted := notification.Meta[persistenthost.ACPUnpromptedMetaKey] == "true"
	if c.proc != nil && c.proc.terminate != nil && !unprompted {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	if unprompted && finished != nil {
		select {
		case <-finished:
		case <-c.closing:
			return nil
		}
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	message := notification.Message
	if message.Type == "system" && message.Subtype == "session_state_changed" && message.State == "running" {
		if message.UUID == "" || c.activeTurn != "" {
			c.mu.Unlock()
			return nil
		}
		id := claudeBackgroundTurnPrefix + message.UUID
		c.activeTurn, c.backgroundTurn = id, id
		c.messages = make(map[string]string)
		c.thoughts = make(map[string]string)
		c.nestedMessages = make(map[string]nestedMessageState)
		c.tools = make(map[string]*toolState)
		c.mu.Unlock()
		startID := stableACPEventID(notification.Meta)
		if startID != "" {
			startID += ":start"
		}
		c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: id, ProviderEventID: startID, ProviderInitiated: true})
		c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerBusy})
		return nil
	}
	id := c.backgroundTurn
	idleWithoutResult := message.Type == "system" && message.Subtype == "session_state_changed" && message.State == "idle"
	if (message.Type != "result" && !idleWithoutResult) || id == "" {
		c.mu.Unlock()
		return nil
	}
	eventID := stableACPEventID(notification.Meta)
	if eventID != "" {
		if c.backgroundTerminalEvents == nil {
			c.backgroundTerminalEvents = make(map[string]bool)
		}
		c.backgroundTerminalEvents[eventID] = true
	}
	c.mu.Unlock()
	c.settleOpenItems(id)
	state := domain.TurnStateCompleted
	var failure error
	if idleWithoutResult {
		state, failure = domain.TurnStateFailed, errors.New("claude background cycle ended without a result")
	} else if message.IsError || (message.Subtype != "success" && message.Subtype != "") {
		state, failure = domain.TurnStateFailed, errors.New("claude background cycle failed")
	} else if strings.TrimSpace(message.StopReason) == "cancelled" {
		state = domain.TurnStateInterrupted
	}
	c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: id, ProviderEventID: eventID, TurnState: state, Err: failure})
	c.mu.Lock()
	c.activeTurn, c.backgroundTurn = "", ""
	c.mu.Unlock()
	c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})
	return nil
}
