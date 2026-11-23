package usage

import (
	"bytes"
	"encoding/json"
	"io"
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
	Info             *struct {
		Last *struct {
			Input int64 `json:"input_tokens"`
		} `json:"last_token_usage"`
	} `json:"info"`
}

// readCodexComposition emits one request per token_count. A request's input
// is the context as of its first model output item, so its own reply is not
// counted as input.
func readCodexComposition(r io.Reader) ([]compositionRequest, error) {
	var state compositionCounts
	var atOutput *compositionCounts
	var out []compositionRequest
	err := eachLine(r, func(raw []byte) {
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
			state[catSystem] = textBytes(p.BaseInstructions)
		case "world_state":
			state[catWorldState] = int64(len(bytes.TrimSpace(line.Payload)))
		case "compacted":
			for _, i := range []int{catInjectedSystem, catHistory, catToolUse, catToolResult, catThinking, catThinkingEncrypted} {
				state[i] = 0
			}
			state[catHistory] += int64(len(p.Message))
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
			if p.Type != "token_count" || p.Info == nil || p.Info.Last == nil {
				return
			}
			input := p.Info.Last.Input
			counts := state
			if atOutput != nil {
				counts = *atOutput
			}
			out = append(out, compositionRequest{timestamp: line.Timestamp, input: &input, counts: counts})
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

func codexItem(state *compositionCounts, p codexCompositionPayload) {
	switch p.Type {
	case "message":
		if p.Role == "developer" || p.Role == "system" {
			state[catInjectedSystem] += max(textBytes(p.Content), 0)
		} else {
			state[catHistory] += max(textBytes(p.Content), 0)
		}
	case "function_call":
		state[catToolUse] += int64(len(p.Arguments))
	case "custom_tool_call":
		state[catToolUse] += int64(len(p.Input))
	case "function_call_output", "custom_tool_call_output":
		state[catToolResult] += max(textBytes(p.Output), 0)
	case "reasoning":
		state[catThinking] += max(textBytes(p.Summary), 0) + max(textBytes(p.Content), 0)
		state[catThinkingEncrypted] += int64(len(p.EncryptedContent))
	case "additional_tools":
		state[catTools] = int64(len(bytes.TrimSpace(p.Tools)))
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
