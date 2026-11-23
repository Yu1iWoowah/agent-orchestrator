package usage

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

type codexCompositionLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexCompositionPayload struct {
	Type             string            `json:"type"`
	Role             string            `json:"role"`
	Content          any               `json:"content"`
	Summary          any               `json:"summary"`
	Output           any               `json:"output"`
	Arguments        string            `json:"arguments"`
	Input            string            `json:"input"`
	EncryptedContent string            `json:"encrypted_content"`
	Tools            json.RawMessage   `json:"tools"`
	BaseInstructions any               `json:"base_instructions"`
	Message          string            `json:"message"`
	Replacement      []json.RawMessage `json:"replacement_history"`
}

// ReadCodexComposition emits one request per token_count that the usage
// parser records as a model usage event, so repeated quota updates and local
// context estimates are not requests. A request's input is the context as of
// its first model output item, so its own reply is not counted as input.
func ReadCodexComposition(r io.Reader) ([]usagesvc.CompositionRequest, error) {
	var state usagesvc.CompositionCounts
	var atOutput *usagesvc.CompositionCounts
	var out []usagesvc.CompositionRequest
	var parser codexParserStateV1
	var parsed parseResult
	err := usagesvc.EachLine(r, func(raw []byte) {
		var line codexCompositionLine
		if json.Unmarshal(raw, &line) != nil {
			return
		}
		var p codexCompositionPayload
		if json.Unmarshal(line.Payload, &p) != nil {
			return
		}
		switch line.Type {
		case "session_meta":
			state[usagesvc.CategorySystem] = textBytes(p.BaseInstructions)
		case "world_state":
			state[usagesvc.CategoryWorldState] = int64(len(bytes.TrimSpace(line.Payload)))
		case "compacted":
			for _, i := range []int{usagesvc.CategoryInjectedSystem, usagesvc.CategoryHistory, usagesvc.CategoryToolUse, usagesvc.CategoryToolResult, usagesvc.CategoryThinking, usagesvc.CategoryThinkingEncrypted} {
				state[i] = 0
			}
			state[usagesvc.CategoryHistory] += int64(len(p.Message))
			for _, item := range p.Replacement {
				var ip codexCompositionPayload
				if json.Unmarshal(item, &ip) == nil {
					codexItem(&state, ip)
				}
			}
		case "response_item":
			if atOutput == nil && codexModelOutput(p) {
				snap := state
				atOutput = &snap
			}
			codexItem(&state, p)
		case "event_msg":
			parsed.Events = parsed.Events[:0]
			parseCodexEvent(domain.UsageSourceContext{}, codexEnvelope(line), &parser, &parsed)
			if len(parsed.Events) == 0 || parsed.Events[0].Tokens.InputTokens == nil {
				return
			}
			input := *parsed.Events[0].Tokens.InputTokens
			counts := state
			if atOutput != nil {
				counts = *atOutput
			}
			out = append(out, usagesvc.CompositionRequest{Timestamp: line.Timestamp, Input: &input, Counts: counts})
			atOutput = nil
		}
	})
	return out, err
}

func codexModelOutput(p codexCompositionPayload) bool {
	switch p.Type {
	case "reasoning", "function_call", "custom_tool_call":
		return true
	case "message":
		return p.Role == "assistant"
	}
	return false
}

func codexItem(state *usagesvc.CompositionCounts, p codexCompositionPayload) {
	switch p.Type {
	case "message":
		if p.Role == "developer" || p.Role == "system" {
			state[usagesvc.CategoryInjectedSystem] += max(textBytes(p.Content), 0)
		} else {
			state[usagesvc.CategoryHistory] += max(textBytes(p.Content), 0)
		}
	case "function_call":
		state[usagesvc.CategoryToolUse] += int64(len(p.Arguments))
	case "custom_tool_call":
		state[usagesvc.CategoryToolUse] += int64(len(p.Input))
	case "function_call_output", "custom_tool_call_output":
		state[usagesvc.CategoryToolResult] += max(textBytes(p.Output), 0)
	case "reasoning":
		state[usagesvc.CategoryThinking] += max(textBytes(p.Summary), 0) + max(textBytes(p.Content), 0)
		state[usagesvc.CategoryThinkingEncrypted] += int64(len(p.EncryptedContent))
	case "additional_tools":
		state[usagesvc.CategoryTools] = int64(len(bytes.TrimSpace(p.Tools)))
	}
}

// textBytes counts visible text in Codex content shapes; -1 when none.
func textBytes(v any) int64 {
	switch v := v.(type) {
	case string:
		return int64(len(v))
	case []any:
		var n int64
		for _, item := range v {
			n += max(textBytes(item), 0)
		}
		return n
	case map[string]any:
		var n int64 = -1
		for _, key := range []string{"text", "content"} {
			if value := textBytes(v[key]); value >= 0 {
				n = max(n, 0) + value
			}
		}
		return n
	}
	return -1
}
