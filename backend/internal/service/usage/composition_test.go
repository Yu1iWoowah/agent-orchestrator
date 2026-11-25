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
	reqs, err := readClaudeComposition(t.Context(), strings.NewReader(claudeCompositionFixture))
	if err != nil || len(reqs) != 2 {
		t.Fatalf("requests = %d, err = %v", len(reqs), err)
	}
	first := CompositionRow(1, reqs[0], false)
	if first.InputTokens == nil || *first.InputTokens != 100 || first.Unresolved != 0 {
		t.Fatalf("first request input = %v unresolved = %d", first.InputTokens, first.Unresolved)
	}
	// 12 + 33 + 39 + 14 = 98 allocated bytes share 100 input tokens; the
	// largest remainders take the 2 leftover tokens, so 12+34+40+14 = 100.
	wantCategory(t, first, "system", 12, ptr(12))
	wantCategory(t, first, "tools", 33, ptr(34))
	wantCategory(t, first, "mcp_tools", 39, ptr(40))
	wantCategory(t, first, "history", 14, ptr(14))

	second := CompositionRow(2, reqs[1], false)
	if second.Unresolved != 1 {
		t.Fatalf("second request unresolved = %d, want only the missing ref", second.Unresolved)
	}
	// 98 + 9 + 11 + 8 = 126 allocated bytes share 200 input tokens:
	// 19+52+62+22+14+18+13 = 200.
	wantCategory(t, second, "system", 12, ptr(19))
	wantCategory(t, second, "tools", 33, ptr(52))
	wantCategory(t, second, "mcp_tools", 39, ptr(62))
	wantCategory(t, second, "history", 14, ptr(22))
	wantCategory(t, second, "tool_use", 9, ptr(14))
	wantCategory(t, second, "tool_result", 11, ptr(18))
	wantCategory(t, second, "injected_system", 8, ptr(13))
}
