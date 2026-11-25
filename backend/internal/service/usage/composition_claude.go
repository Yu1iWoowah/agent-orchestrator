package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// claudeCompositionLine decodes only the fields the request chain needs.
type claudeCompositionLine struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	Message    json.RawMessage `json:"message"`
	Hash       string          `json:"hash"`
	ShapeHash  json.RawMessage `json:"shapeHash"`
	Shape      json.RawMessage `json:"shape"`
	ID         string          `json:"id"`
	Base       string          `json:"base"`
	Keep       int             `json:"keep"`
	Tail       []string        `json:"tail"`
	Timestamp  string          `json:"timestamp"`
	RequestRef string          `json:"requestRef"`
}

type claudeAPIRequest struct {
	id, shapeHash, timestamp string
	base                     *claudeAPIRequest
	keep                     int
	tail                     []string
	unresolved               int
	prefix                   claudeMessageCounts
}

type claudeMessageCounts struct {
	counts     CompositionCounts
	unresolved int
}

func (c *claudeMessageCounts) addMessages(hashes []string, messages map[string]CompositionCounts) {
	for _, hash := range hashes {
		if counts, ok := messages[hash]; ok {
			c.counts.add(counts)
		} else {
			c.unresolved++
		}
	}
}

// readClaudeComposition rebuilds each api-request's messages from its
// base/keep/tail chain. Tail entries name api-request-blob records or
// transcript messages by Claude Code's canonical message hash.
func readClaudeComposition(ctx context.Context, r io.Reader) ([]CompositionRequest, error) {
	messages := map[string]CompositionCounts{}
	shapes := map[string]CompositionCounts{}
	usage := map[string]int64{}
	requests := map[string]*claudeAPIRequest{}
	var order []*claudeAPIRequest

	var run struct {
		role, id string
		blocks   []json.RawMessage
		counts   CompositionCounts
		active   bool
	}
	err := EachLine(ctx, r, func(raw []byte) {
		var line claudeCompositionLine
		if json.Unmarshal(raw, &line) != nil {
			return
		}
		// An invalid shape reference leaves the request and its messages usable.
		var shapeHash string
		if line.ShapeHash != nil {
			_ = json.Unmarshal(line.ShapeHash, &shapeHash)
		}
		switch line.Type {
		case "system":
			if line.Subtype == "compact_boundary" {
				run.active = false
			}
		case "api-request-blob":
			if role, blocks, ok := claudeMessage(line.Message); ok && line.Hash != "" {
				messages[line.Hash] = claudeBlockCounts(role, blocks)
			}
		case "api-request-shape":
			if counts, ok := claudeShapeCounts(line.Shape); ok && shapeHash != "" {
				shapes[shapeHash] = counts
			}
		case "api-request":
			if line.ID == "" {
				return
			}
			req := &claudeAPIRequest{id: line.ID, shapeHash: shapeHash, timestamp: line.Timestamp, tail: line.Tail}
			if line.Keep > 0 {
				if parent := requests[line.Base]; parent != nil && parent.keep+len(parent.tail) >= line.Keep {
					req.base, req.keep = parent, line.Keep
					req.unresolved = parent.unresolved
				} else {
					req.unresolved += line.Keep
				}
			}
			requests[line.ID] = req
			order = append(order, req)
		case "user", "assistant":
			if line.RequestRef != "" {
				if _, seen := usage[line.RequestRef]; !seen {
					if input, ok := claudeInputTokens(line.Message); ok {
						usage[line.RequestRef] = input
					}
				}
			}
			role, blocks, ok := claudeMessage(line.Message)
			if !ok {
				run.active = false
				return
			}
			id := claudeMessageID(line.Message)
			same := run.active && run.role == role && (role == "user" || (id != "" && run.id == id))
			if !same {
				run.role, run.id, run.blocks, run.counts, run.active = role, id, nil, CompositionCounts{}, true
			}
			run.blocks = append(run.blocks, blocks...)
			run.counts.add(claudeBlockCounts(role, blocks))
			messages[claudeMessageHash(role, run.blocks)] = run.counts
		}
	})
	if err != nil {
		return nil, err
	}
	out := make([]CompositionRequest, 0, len(order))
	for _, req := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if base := req.base; base != nil {
			// Bases are earlier requests. Reuse their resolved prefix counts,
			// following the chain only when keep cuts into a retained prefix.
			for req.keep < base.keep {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				base = base.base
			}
			req.prefix = base.prefix
			req.prefix.addMessages(base.tail[:req.keep-base.keep], messages)
		}
		total := req.prefix
		total.addMessages(req.tail, messages)
		row := CompositionRequest{Timestamp: req.timestamp, Unresolved: req.unresolved + total.unresolved, Counts: total.counts}
		if shape, ok := shapes[req.shapeHash]; ok {
			row.Counts.add(shape)
		} else {
			row.Counts[CategorySystem] = -1
			row.Counts[CategoryTools] = -1
			row.Counts[CategoryMCPTools] = -1
		}
		if input, ok := usage[req.id]; ok {
			row.Input = &input
		}
		out = append(out, row)
	}
	return out, nil
}

// claudeMessage normalizes content to a block list, as Claude Code hashes it.
func claudeMessage(raw json.RawMessage) (string, []json.RawMessage, bool) {
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Role == "" || len(msg.Content) == 0 || string(msg.Content) == "null" {
		return "", nil, false
	}
	var text string
	if json.Unmarshal(msg.Content, &text) == nil {
		block, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		return msg.Role, []json.RawMessage{block}, true
	}
	var blocks []json.RawMessage
	if json.Unmarshal(msg.Content, &blocks) != nil {
		blocks = []json.RawMessage{msg.Content}
	}
	return msg.Role, blocks, true
}

func claudeMessageID(raw json.RawMessage) string {
	var msg struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &msg)
	return msg.ID
}

func claudeInputTokens(raw json.RawMessage) (int64, bool) {
	var msg struct {
		Usage *struct {
			Input       int64 `json:"input_tokens"`
			CacheCreate int64 `json:"cache_creation_input_tokens"`
			CacheRead   int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Usage == nil {
		return 0, false
	}
	total, err := checkedUsageAdd("Claude input tokens", msg.Usage.Input, msg.Usage.CacheCreate)
	if err != nil {
		return 0, false
	}
	total, err = checkedUsageAdd("Claude input tokens", total, msg.Usage.CacheRead)
	return total, err == nil
}

func claudeMessageHash(role string, blocks []json.RawMessage) string {
	var b bytes.Buffer
	b.WriteString(`{"content":[`)
	for i, block := range blocks {
		if i > 0 {
			b.WriteByte(',')
		}
		writeCanonicalRaw(&b, block)
	}
	b.WriteString(`],"role":`)
	writeJSString(&b, role)
	b.WriteByte('}')
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

func claudeBlockCounts(role string, blocks []json.RawMessage) CompositionCounts {
	var c CompositionCounts
	for _, raw := range blocks {
		var block struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Data     string          `json:"data"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Content  json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(raw, &block)
		switch {
		case role == "system":
			c[CategoryInjectedSystem] += int64(len(block.Text))
		case block.Type == "text":
			c[CategoryHistory] += int64(len(block.Text))
		case block.Type == "tool_use":
			c[CategoryToolUse] += canonicalLen(block.Input)
		case block.Type == "tool_result":
			c[CategoryToolResult] += claudeResultBytes(block.Content)
		case block.Type == "thinking":
			c[CategoryThinking] += int64(len(block.Thinking))
		case block.Type == "redacted_thinking":
			c[CategoryThinkingEncrypted] += int64(len(block.Data))
		case block.Type == "tool_addition":
			if strings.Contains(string(raw), `"mcp__`) {
				c[CategoryMCPTools] += canonicalLen(raw)
			} else {
				c[CategoryTools] += canonicalLen(raw)
			}
		default:
			c[CategoryHistory] += canonicalLen(raw)
		}
	}
	return c
}

func claudeResultBytes(raw json.RawMessage) int64 {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return int64(len(text))
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var n int64
		for _, part := range parts {
			n += int64(len(part.Text))
		}
		return n
	}
	return canonicalLen(raw)
}

func claudeShapeCounts(raw json.RawMessage) (CompositionCounts, bool) {
	var c CompositionCounts
	var shape *struct {
		System json.RawMessage   `json:"system"`
		Tools  []json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(raw, &shape) != nil || shape == nil {
		return c, false
	}
	c[CategorySystem] = claudeResultBytes(shape.System)
	for _, tool := range shape.Tools {
		var named struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(tool, &named)
		if strings.HasPrefix(named.Name, "mcp__") {
			c[CategoryMCPTools] += canonicalLen(tool)
		} else {
			c[CategoryTools] += canonicalLen(tool)
		}
	}
	return c, true
}

func canonicalLen(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var b bytes.Buffer
	writeCanonicalRaw(&b, raw)
	return int64(b.Len())
}

// writeCanonicalRaw mirrors Claude Code's request hash serializer: sorted
// keys, cache_control omitted, JSON.stringify string escaping, numbers as
// written.
func writeCanonicalRaw(b *bytes.Buffer, raw json.RawMessage) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		b.Write(raw)
		return
	}
	writeCanonical(b, v)
}

func writeCanonical(b *bytes.Buffer, v any) {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case json.Number:
		b.WriteString(v.String())
	case string:
		writeJSString(b, v)
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, item)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			if k != "cache_control" {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSString(b, k)
			b.WriteByte(':')
			writeCanonical(b, v[k])
		}
		b.WriteByte('}')
	}
}

func utf16Less(a, b string) bool {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

// writeJSString escapes like JSON.stringify: no HTML or U+2028 escaping.
func writeJSString(b *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[r>>4])
				b.WriteByte(hexDigits[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
