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
	preceding, _ := notification.Meta[persistenthost.ACPPrecedingPromptMetaKey].(string)
	waitForSettlement := preceding != "" && preceding != c.terminalEventID
	unprompted := notification.Meta[persistenthost.ACPUnpromptedMetaKey] == "true"
	if c.proc != nil && c.proc.terminate != nil && !unprompted {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()
	if unprompted && waitForSettlement && finished != nil {
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
		c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: id, ProviderConversationID: c.ProviderConversationID(), ProviderEventID: startID, ProviderInitiated: true})
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
		if c.unpromptedReceipts == nil {
			c.unpromptedReceipts = make(map[string]string)
		}
		c.unpromptedReceipts[eventID] = eventID
	}
	interrupt := c.interrupt
	if c.activeTurn == id {
		c.settlingTurn = id
	}
	c.mu.Unlock()
	c.settleOpenItems(id)
	interruptedLocally := false
	if interrupt != nil && interrupt.turnID == id {
		<-interrupt.done
		interruptedLocally = interrupt.err == nil
	}
	state := domain.TurnStateCompleted
	var failure error
	if interruptedLocally {
		state = domain.TurnStateInterrupted
	} else if idleWithoutResult {
		state, failure = domain.TurnStateFailed, errors.New("claude background cycle ended without a result")
	} else if message.IsError || (message.Subtype != "success" && message.Subtype != "") {
		state, failure = domain.TurnStateFailed, errors.New("claude background cycle failed")
	} else if strings.TrimSpace(message.StopReason) == "cancelled" {
		state = domain.TurnStateInterrupted
	}
	c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: id, ProviderConversationID: c.ProviderConversationID(), ProviderEventID: eventID, TurnState: state, Err: failure})
	c.mu.Lock()
	released := c.activeTurn == id
	if released {
		c.activeTurn, c.settlingTurn = "", ""
		if c.interrupt == interrupt {
			c.interrupt = nil
		}
	}
	if c.backgroundTurn == id {
		c.backgroundTurn = ""
	}
	c.mu.Unlock()
	if released {
		c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})
	}
	return nil
}

// Unmodeled notifications have no terminal turn receipt. Put their ACK behind
// all preceding projections; acknowledging from this reader could discard a
// completed background cycle before the controller has committed it.
func (c *conversation) checkpointUnprompted(raw json.RawMessage) {
	var notification struct {
		Meta map[string]any `json:"_meta"`
	}
	if json.Unmarshal(raw, &notification) != nil || notification.Meta[persistenthost.ACPUnpromptedMetaKey] != "true" {
		return
	}
	id := stableACPEventID(notification.Meta)
	if id == "" {
		return
	}
	c.mu.Lock()
	if c.backgroundTurn != "" || c.unpromptedReceipts[id] != "" {
		c.mu.Unlock()
		return
	}
	if c.unpromptedReceipts == nil {
		c.unpromptedReceipts = make(map[string]string)
	}
	receipt := id + ":checkpoint"
	c.unpromptedReceipts[receipt] = id
	c.mu.Unlock()
	c.emit(ports.ChatEvent{Kind: ports.ChatEventProviderCheckpoint, ProviderEventID: receipt})
}
