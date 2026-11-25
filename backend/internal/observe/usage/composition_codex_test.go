package usage

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
)

func compositionCategory(t *testing.T, row domain.UsageCompositionRequest, name string) domain.UsageCompositionCategory {
	t.Helper()
	for _, c := range row.Categories {
		if c.Category == name {
			return c
		}
	}
	t.Fatalf("category %s missing", name)
	return domain.UsageCompositionCategory{}
}

func wantCompositionCategory(t *testing.T, row domain.UsageCompositionRequest, name string, bytes int64, tokens *int64) {
	t.Helper()
	c := compositionCategory(t, row, name)
	if c.Bytes == nil || *c.Bytes != bytes {
		t.Errorf("request %d %s bytes = %v, want %d", row.Ordinal, name, c.Bytes, bytes)
	}
	if (tokens == nil) != (c.Tokens == nil) || (tokens != nil && *c.Tokens != *tokens) {
		t.Errorf("request %d %s tokens = %v, want %v", row.Ordinal, name, c.Tokens, tokens)
	}
}

const codexCompositionFixture = `{"timestamp":"t0","type":"session_meta","payload":{"id":"x","base_instructions":{"text":"BASE"}}}
{"timestamp":"t0","type":"response_item","payload":{"type":"additional_tools","tools":[{"name":"t"}]}}
{"timestamp":"t0","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]}}
{"timestamp":"t0","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}
{"timestamp":"t0","type":"world_state","payload":{"full":{"a":1}}}
{"timestamp":"t0","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"ENC"}}
{"timestamp":"t0","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}
{"timestamp":"t1","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":50},"last_token_usage":{"input_tokens":50}}}}
{"timestamp":"t1","type":"response_item","payload":{"type":"function_call_output","output":"out"}}
{"timestamp":"t1","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}
{"timestamp":"t2","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":130},"last_token_usage":{"input_tokens":80}}}}
`

func TestCodexCompositionUsesTokenCountAndAdditionalTools(t *testing.T) {
	reqs, err := ReadCodexComposition(t.Context(), strings.NewReader(codexCompositionFixture))
	if err != nil || len(reqs) != 2 {
		t.Fatalf("requests = %d, err = %v", len(reqs), err)
	}
	first := usagesvc.CompositionRow(1, reqs[0], true)
	if first.InputTokens == nil || *first.InputTokens != 50 {
		t.Fatalf("first request input = %v", first.InputTokens)
	}
	// Input is the context before the request's own reasoning/tool call:
	// 4 + 14 + 3 + 2 = 23 allocated bytes share 50 tokens: 9+30+7+4 = 50.
	wantCompositionCategory(t, first, "system", 4, int64Ptr(9))
	wantCompositionCategory(t, first, "tools", 14, int64Ptr(30))
	wantCompositionCategory(t, first, "injected_system", 3, int64Ptr(7))
	wantCompositionCategory(t, first, "history", 2, int64Ptr(4))
	wantCompositionCategory(t, first, "world_state", 16, nil)
	if c := compositionCategory(t, first, "mcp_tools"); c.Bytes != nil || c.Note != "unknown" {
		t.Fatalf("codex MCP = %+v, want UNKNOWN", c)
	}

	second := usagesvc.CompositionRow(2, reqs[1], true)
	// 23 + 2 (tool call) + 3 (tool output) + 3 (encrypted reasoning) = 31
	// allocated bytes share 80 tokens: 10+36+8+5+5+8+8 = 80.
	wantCompositionCategory(t, second, "system", 4, int64Ptr(10))
	wantCompositionCategory(t, second, "tools", 14, int64Ptr(36))
	wantCompositionCategory(t, second, "injected_system", 3, int64Ptr(8))
	wantCompositionCategory(t, second, "history", 2, int64Ptr(5))
	wantCompositionCategory(t, second, "tool_use", 2, int64Ptr(5))
	wantCompositionCategory(t, second, "tool_result", 3, int64Ptr(8))
	wantCompositionCategory(t, second, "thinking_encrypted", 3, int64Ptr(8))
	if c := compositionCategory(t, second, "thinking_encrypted"); c.Note != "encrypted_hidden" {
		t.Fatalf("encrypted thinking note = %q", c.Note)
	}
}
