package usage

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func categoryOf(t *testing.T, row domain.UsageCompositionRequest, name string) domain.UsageCompositionCategory {
	t.Helper()
	for _, c := range row.Categories {
		if c.Category == name {
			return c
		}
	}
	t.Fatalf("category %s missing", name)
	return domain.UsageCompositionCategory{}
}

func wantCategory(t *testing.T, row domain.UsageCompositionRequest, name string, bytes int64, tokens *int64) {
	t.Helper()
	c := categoryOf(t, row, name)
	if c.Bytes == nil || *c.Bytes != bytes {
		t.Errorf("request %d %s bytes = %v, want %d", row.Ordinal, name, c.Bytes, bytes)
	}
	if (tokens == nil) != (c.Tokens == nil) || (tokens != nil && *c.Tokens != *tokens) {
		t.Errorf("request %d %s tokens = %v, want %v", row.Ordinal, name, c.Tokens, tokens)
	}
}

func ptr(n int64) *int64 { return &n }

// Synthetic transcript: hashes are Claude Code's canonical message hashes
// (sorted keys, cache_control omitted, JSON.stringify escaping).
const claudeCompositionFixture = `{"type":"user","message":{"role":"user","content":"hello <b>&</b>"}}
{"type":"api-request-shape","shapeHash":"S","shape":{"system":[{"type":"text","text":"SYSTEMSYSTEM"}],"tools":[{"name":"Read","input_schema":{}},{"name":"mcp__ao__x","input_schema":{}}]}}
{"type":"api-request","id":"r1","shapeHash":"S","keep":0,"tail":["d7f0dbe264f3fa24caedf4f78e2b1f2c723a5fa450224c604e09cb776da4d978"],"timestamp":"t1"}
{"type":"assistant","requestRef":"r1","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"tu","name":"Read","input":{"p":"a"},"cache_control":{"type":"ephemeral"}}],"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":70}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu","content":"result-text"}]}}
{"type":"api-request-blob","hash":"B1","message":{"role":"system","content":[{"type":"text","text":"reminder"}]}}
{"type":"api-request","id":"r2","base":"r1","keep":1,"shapeHash":"S","tail":["9ad2103c481c1c6823976fd838c469580ae6e5db6a7d6de24693c1afe21cd92e","e064d8a8bcc7ff26cb745306c86fedec8fa0e854a733702c201f1c7313b04c67","B1","missing"],"timestamp":"t2"}
{"type":"assistant","requestRef":"r2","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":200,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
`

func TestClaudeCompositionRebuildsChainAndAllocatesInput(t *testing.T) {
	reqs, err := readClaudeComposition(strings.NewReader(claudeCompositionFixture))
	if err != nil || len(reqs) != 2 {
		t.Fatalf("requests = %d, err = %v", len(reqs), err)
	}
	first := compositionRow(1, reqs[0], false)
	if first.InputTokens == nil || *first.InputTokens != 100 || first.Unresolved != 0 {
		t.Fatalf("first request input = %v unresolved = %d", first.InputTokens, first.Unresolved)
	}
	// 12 + 33 + 39 + 14 = 98 allocated bytes share 100 input tokens.
	wantCategory(t, first, "system", 12, ptr(12))
	wantCategory(t, first, "tools", 33, ptr(33))
	wantCategory(t, first, "mcp_tools", 39, ptr(39))
	wantCategory(t, first, "history", 14, ptr(14))

	second := compositionRow(2, reqs[1], false)
	if second.Unresolved != 1 {
		t.Fatalf("second request unresolved = %d, want only the missing ref", second.Unresolved)
	}
	// 98 + 9 + 11 + 8 = 126 allocated bytes share 200 input tokens.
	wantCategory(t, second, "history", 14, ptr(22))
	wantCategory(t, second, "tool_use", 9, ptr(14))
	wantCategory(t, second, "tool_result", 11, ptr(17))
	wantCategory(t, second, "injected_system", 8, ptr(12))
}

const codexCompositionFixture = `{"timestamp":"t0","type":"session_meta","payload":{"id":"x","base_instructions":{"text":"BASE"}}}
{"timestamp":"t0","type":"response_item","payload":{"type":"additional_tools","tools":[{"name":"t"}]}}
{"timestamp":"t0","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev"}]}}
{"timestamp":"t0","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}
{"timestamp":"t0","type":"world_state","payload":{"full":{"a":1}}}
{"timestamp":"t0","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"ENC"}}
{"timestamp":"t0","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}
{"timestamp":"t1","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":50}}}}
{"timestamp":"t1","type":"response_item","payload":{"type":"function_call_output","output":"out"}}
{"timestamp":"t1","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}
{"timestamp":"t2","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":80}}}}
`

func TestCodexCompositionUsesTokenCountAndAdditionalTools(t *testing.T) {
	reqs, err := readCodexComposition(strings.NewReader(codexCompositionFixture))
	if err != nil || len(reqs) != 2 {
		t.Fatalf("requests = %d, err = %v", len(reqs), err)
	}
	first := compositionRow(1, reqs[0], true)
	if first.InputTokens == nil || *first.InputTokens != 50 {
		t.Fatalf("first request input = %v", first.InputTokens)
	}
	// Input is the context before the request's own reasoning/tool call:
	// 4 + 14 + 3 + 2 = 23 allocated bytes share 50 tokens.
	wantCategory(t, first, "system", 4, ptr(8))
	wantCategory(t, first, "tools", 14, ptr(30))
	wantCategory(t, first, "injected_system", 3, ptr(6))
	wantCategory(t, first, "history", 2, ptr(4))
	wantCategory(t, first, "world_state", 16, nil)
	if c := categoryOf(t, first, "mcp_tools"); c.Bytes != nil || c.Note != "unknown" {
		t.Fatalf("codex MCP = %+v, want UNKNOWN", c)
	}

	second := compositionRow(2, reqs[1], true)
	// 23 + 2 (tool call) + 3 (tool output) + 3 (encrypted reasoning) = 31
	// allocated bytes share 80 tokens.
	wantCategory(t, second, "tool_use", 2, ptr(5))
	wantCategory(t, second, "tool_result", 3, ptr(7))
	wantCategory(t, second, "thinking_encrypted", 3, ptr(7))
	if c := categoryOf(t, second, "thinking_encrypted"); c.Note != "encrypted_hidden" {
		t.Fatalf("encrypted thinking note = %q", c.Note)
	}
}
