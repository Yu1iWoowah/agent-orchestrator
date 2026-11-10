package rca

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	claudeBatchBytes   = 1 << 20
	claudeRecordBytes  = 256 << 10
	claudeBodyBytes    = 256 << 10
	claudeRecords      = 256
	claudeIndexEntries = 4096
	claudeIndexBytes   = 16 << 20
)

var errClaudeWire = errors.New("invalid_otlp")

// Native identity and raw content live only inside the admitted request and
// bounded transient capture indices. No member of this type is a DTO or store.
type claudeEvent struct {
	name, session, bodyID, requestID, messageID, sequence string
	body                                                  []byte
	truncated                                             bool
	unprovedChild                                         bool
}

// protoField reads a tag and (for wire type 2) a length, without consuming the
// value. In particular an oversized LogRecord is refused before its body/tail.
func protoField(r *io.LimitedReader) (field, wire int, size int64, err error) {
	readVarint := func() (uint64, error) {
		var raw [10]byte
		for i := range raw {
			if r.N <= 0 {
				return 0, errClaudeWire
			}
			if _, e := io.ReadFull(r, raw[i:i+1]); e != nil {
				return 0, errClaudeWire
			}
			if raw[i] < 128 {
				n, count := binary.Uvarint(raw[:i+1])
				if count <= 0 {
					return 0, errClaudeWire
				}
				return n, nil
			}
		}
		return 0, errClaudeWire
	}
	tag, err := readVarint()
	if err != nil || tag>>3 == 0 || tag>>3 > (1<<29)-1 {
		return 0, 0, 0, errClaudeWire
	}
	field, wire = int(tag>>3), int(tag&7)
	switch wire {
	case 0:
		_, err = readVarint()
	case 1:
		size = 8
	case 2:
		var encoded uint64
		encoded, err = readVarint()
		if encoded > claudeBatchBytes {
			return 0, 0, 0, errClaudeWire
		}
		size = int64(encoded)
	case 5:
		size = 4
	default:
		err = errClaudeWire
	}
	if err != nil || size > r.N {
		return 0, 0, 0, errClaudeWire
	}
	return field, wire, size, nil
}

func skipProto(r *io.LimitedReader, n int64) error {
	if n < 0 || n > r.N {
		return errClaudeWire
	}
	if _, err := io.CopyN(io.Discard, r, n); err != nil {
		return errClaudeWire
	}
	return nil
}

// readClaudeOTLP decodes only ExportLogsServiceRequest -> ResourceLogs ->
// ScopeLogs -> LogRecord. It holds at most one bounded raw record, never a batch
// of decoded bodies. Unknown protobuf metadata is skipped without retaining it.
func readClaudeOTLP(ctx context.Context, source io.Reader, length int64, consume func(claudeEvent) string) string {
	if length < 0 || length > claudeBatchBytes {
		return "batch_limit"
	}
	reader := &io.LimitedReader{R: source, N: length}
	records := 0
	var walk func(*io.LimitedReader, int) string
	walk = func(r *io.LimitedReader, level int) string {
		for r.N > 0 {
			if ctx.Err() != nil {
				return "canceled"
			}
			field, wire, n, err := protoField(r)
			if err != nil {
				return "invalid_otlp"
			}
			wanted := (level == 0 && field == 1) || (level == 1 && field == 2) || (level == 2 && field == 2)
			if !wanted {
				if skipProto(r, n) != nil {
					return "invalid_otlp"
				}
				continue
			}
			if wire != 2 {
				return "invalid_otlp"
			}
			if level < 2 {
				child := &io.LimitedReader{R: r, N: n}
				if reason := walk(child, level+1); reason != "" {
					return reason
				}
				if child.N != 0 {
					return "invalid_otlp"
				}
				continue
			}
			records++
			if records > claudeRecords {
				return "record_count_limit"
			}
			if n > claudeRecordBytes {
				return "record_limit"
			}
			raw := make([]byte, n)
			if _, err := io.ReadFull(r, raw); err != nil {
				return "source_read_failed"
			}
			event, reason := decodeClaudeRecord(raw)
			if reason != "" {
				return reason
			}
			if reason = consume(event); reason != "" {
				return reason
			}
		}
		return ""
	}
	if reason := walk(reader, 0); reason != "" {
		return reason
	}
	if records == 0 {
		return "unknown_schema"
	}
	return ""
}

func protoBytes(data []byte, field int) ([]byte, bool) {
	r := &io.LimitedReader{R: bytes.NewReader(data), N: int64(len(data))}
	var found []byte
	for r.N > 0 {
		f, w, n, e := protoField(r)
		if e != nil {
			return nil, false
		}
		if f == field {
			if w != 2 || found != nil {
				return nil, false
			}
			found = make([]byte, n)
			if _, e = io.ReadFull(r, found); e != nil {
				return nil, false
			}
		} else if skipProto(r, n) != nil {
			return nil, false
		}
	}
	return found, found != nil
}

func decodeClaudeRecord(raw []byte) (claudeEvent, string) {
	var event claudeEvent
	r := &io.LimitedReader{R: bytes.NewReader(raw), N: int64(len(raw))}
	seen := map[string]bool{}
	for r.N > 0 {
		field, wire, n, err := protoField(r)
		if err != nil {
			return event, "invalid_otlp"
		}
		if field != 6 {
			if skipProto(r, n) != nil {
				return event, "invalid_otlp"
			}
			continue
		}
		if wire != 2 {
			return event, "invalid_otlp"
		}
		attr := make([]byte, n)
		if _, err = io.ReadFull(r, attr); err != nil {
			return event, "invalid_otlp"
		}
		key, ok := protoBytes(attr, 1)
		if !ok || !utf8.Valid(key) {
			return event, "invalid_otlp"
		}
		// Unknown attributes (including external project/role/account) confer no
		// authority and are not copied into any capture index or projection.
		name := string(key)
		switch name {
		case "query_source", "subagent_id", "parent_session_id", "agent_id":
			event.unprovedChild = true
			continue
		}
		switch name {
		case "event.name", "session.id", "request_body_id", "request_id", "message.id", "message.uuid", "body", "body_truncated":
		default:
			continue
		}
		if seen[name] {
			return event, "unknown_schema"
		}
		seen[name] = true
		value, ok := protoBytes(attr, 2)
		if !ok {
			return event, "unknown_schema"
		}
		text, ok := protoBytes(value, 1)
		if name == "body_truncated" && !ok {
			// AnyValue bool_value is field 2, varint. Values other than 0/1 are
			// malformed. The whole message must be that one scalar.
			if len(value) != 2 || value[0] != 16 || value[1] > 1 {
				return event, "unknown_schema"
			}
			event.truncated = value[1] == 1
			continue
		}
		if !ok || !utf8.Valid(text) {
			return event, "unknown_schema"
		}
		if name == "body" {
			if len(text) > claudeBodyBytes {
				return event, "body_limit"
			}
			event.body = text
			continue
		}
		if len(text) > 256 {
			return event, "identity_limit"
		}
		switch name {
		case "event.name":
			event.name = strings.TrimPrefix(string(text), "claude_code.")
		case "session.id":
			event.session = string(text)
		case "request_body_id":
			event.bodyID = string(text)
		case "request_id":
			event.requestID = string(text)
		case "message.id", "message.uuid":
			if event.messageID != "" && event.messageID != string(text) {
				return event, "identity_conflict"
			}
			event.messageID = string(text)
		case "event.sequence":
			event.sequence = string(text)
		case "body_truncated":
			if string(text) != "true" && string(text) != "false" {
				return event, "unknown_schema"
			}
			event.truncated = string(text) == "true"
		}
	}
	if event.name == "" || event.session == "" {
		return event, "unknown_schema"
	}
	return event, ""
}

// classifyClaudeBody is a raw-to-numeric boundary. It never returns a string
// from the request; tool names and native IDs are deliberately not fields.
func classifyClaudeBody(body []byte, truncated bool) (recording, int64, string) {
	out := recording{Method: "utf8_bytes_div4_v1", Status: "partial", Reasons: []string{"retained_context_unknown"}, Buckets: emptyBuckets(), Unpaired: emptyBuckets(), Turns: []recordedTurn{}}
	if len(body) > claudeBodyBytes {
		return out, 0, "body_limit"
	}
	if !utf8.Valid(body) {
		return out, 0, "invalid_record"
	}
	if exceedsDepth(body, 32) || exceedsClaudeContainers(body) {
		return out, 0, "parse_limit"
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return out, 0, "unknown_schema"
	}
	if truncated {
		out.gap("source_truncated")
	}
	text := func(raw json.RawMessage, category int) {
		if len(raw) == 0 {
			return
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			out.gap("unknown_schema")
			return
		}
		if unsupportedText(v) {
			out.gap("unsupported_content")
		}
		out.add(category, textBytes(v), -1)
	}
	text(request["system"], 0)
	var tools []json.RawMessage
	var mcp int64
	if raw, ok := request["tools"]; ok {
		if json.Unmarshal(raw, &tools) != nil {
			out.gap("unknown_schema")
		} else {
			if len(tools) == 0 {
				out.add(1, 0, -1)
			}
			for _, rawTool := range tools {
				var tool map[string]json.RawMessage
				if json.Unmarshal(rawTool, &tool) != nil || tool == nil {
					out.gap("unknown_schema")
					continue
				}
				schema := tool["input_schema"]
				var object map[string]json.RawMessage
				if json.Unmarshal(schema, &object) != nil || object == nil {
					out.gap("unknown_schema")
					continue
				}
				out.add(1, int64(len(rawTool)), -1)
				var name string
				_ = json.Unmarshal(tool["name"], &name)
				if strings.HasPrefix(name, "mcp__") {
					mcp += int64(len(rawTool))
				}
			}
		}
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if raw, ok := request["messages"]; ok {
		if json.Unmarshal(raw, &messages) != nil {
			out.gap("unknown_schema")
		} else {
			for _, message := range messages {
				category := 2
				if message.Role == "system" || message.Role == "developer" {
					category = 0
				} else if message.Role != "user" && message.Role != "assistant" {
					out.gap("unknown_schema")
					continue
				}
				var scalar string
				if json.Unmarshal(message.Content, &scalar) == nil {
					out.add(category, int64(len(scalar)), -1)
					continue
				}
				var blocks []map[string]json.RawMessage
				if json.Unmarshal(message.Content, &blocks) != nil {
					out.gap("unknown_schema")
					continue
				}
				for _, block := range blocks {
					var kind string
					_ = json.Unmarshal(block["type"], &kind)
					switch kind {
					case "text":
						text(block["text"], category)
					case "tool_result":
						text(block["content"], 3)
					case "thinking":
						text(block["thinking"], 4)
					case "redacted_thinking":
						out.gap("hidden_thinking")
					case "tool_use":
						if raw := block["input"]; len(raw) > 0 {
							out.add(5, int64(len(raw)), -1)
						} else {
							out.gap("missing_key")
						}
					default:
						out.gap("unsupported_content")
					}
				}
			}
		}
	}
	for key := range request {
		switch key {
		case "system", "tools", "messages", "model", "max_tokens", "stream", "temperature", "top_p", "top_k", "stop_sequences", "metadata", "thinking", "tool_choice", "service_tier", "betas", "context_management", "output_config":
		default:
			out.gap("unknown_fields")
		}
	}
	return out, mcp, ""
}

// Bound decoding containers before json.Unmarshal. This conservative charge is
// separate from raw bytes and the transient numeric/identity index budget.
func exceedsClaudeContainers(body []byte) bool {
	charge := len(body) * 2
	inString, escape := false, false
	for _, c := range body {
		if inString {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			charge += 128
		case ',', ':':
			charge += 64
		}
		if charge > 4<<20 {
			return true
		}
	}
	return false
}
