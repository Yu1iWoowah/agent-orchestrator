// Package rca estimates recorded content without changing canonical usage.
package rca

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const sourceVersion = "0.167.0"

type readLimits struct {
	Window, Line int64
	Depth        int
}

var defaultReadLimits = readLimits{Window: 1024 << 20, Line: 64 << 20, Depth: 32}

// Bucket contains only numbers and a fixed category, never content or names.
type Bucket = domain.RCABucket

type recordedTurn = domain.RCARecordedTurn

type recording domain.RCARecordedSnapshot

func emptyBuckets() [6]Bucket {
	var out [6]Bucket
	for i, name := range [...]string{"system_developer", "tool_schema", "conversation_history", "tool_result", "retained_thinking", "other_observed"} {
		out[i].Category = name
	}
	return out
}

func hasReason(r recording, reason string) bool {
	for _, s := range r.Reasons {
		if s == reason {
			return true
		}
	}
	return false
}

func (r *recording) gap(reason string) {
	if !hasReason(*r, reason) {
		r.Reasons = append(r.Reasons, reason)
	}
	if r.Status != "unavailable" {
		r.Status = "partial"
	}
}

func (r *recording) reject(reason string) {
	r.gap(reason)
	r.Status = "unavailable"
	r.Buckets, r.Unpaired, r.Turns = emptyBuckets(), emptyBuckets(), []recordedTurn{}
}

func (r *recording) add(category int, n int64, turn int) {
	if n < 0 {
		r.gap("missing_key")
		return
	}
	add := func(b *Bucket) {
		if b.Bytes == nil {
			var zero int64
			b.Bytes = &zero
			var tokens int64
			b.EstimatedTokens = &tokens
		}
		*b.Bytes += n
		*b.EstimatedTokens += (n + 3) / 4
	}
	add(&r.Buckets[category])
	if turn >= 0 {
		add(&r.Turns[turn].Buckets[category])
	} else {
		add(&r.Unpaired[category])
	}
}

// readCodex sees only an already-authorized descriptor and captured EOF. It
// never opens files, resolves roots, follows parents, or emits diagnostic errors.
func readCodex(ctx context.Context, file io.ReaderAt, eof int64, nativeID string, limits readLimits) recording {
	r := recording{Method: "utf8_bytes_div4_v1", Status: "ready", Reasons: []string{}, Buckets: emptyBuckets(), Unpaired: emptyBuckets(), Turns: []recordedTurn{}, EndOffset: eof}
	if eof < 0 || limits.Window <= 0 || limits.Line <= 0 || limits.Depth <= 0 {
		r.reject("invalid_snapshot")
		return r
	}
	if eof > limits.Window {
		r.StartOffset = eof - limits.Window
		r.gap("window_head_gap")
	}
	stream := bufio.NewReaderSize(io.NewSectionReader(file, r.StartOffset, eof-r.StartOffset), 32<<10)
	// The first line at a nonzero seek has no proven boundary. Discard it
	// conservatively; even a one-byte probe before StartOffset exceeds the window.
	if r.StartOffset > 0 {
		for {
			if ctx.Err() != nil {
				r.reject("canceled")
				return r
			}
			_, err := stream.ReadSlice('\n')
			if !errors.Is(err, bufio.ErrBufferFull) {
				if err != nil && !errors.Is(err, io.EOF) {
					r.reject("source_read_failed")
				}
				break
			}
		}
	}
	verified, currentTurn := false, -1
	currentTurnKey := ""
	for {
		if ctx.Err() != nil {
			r.reject("canceled")
			return r
		}
		line, oversized, complete, err := readLine(ctx, stream, limits.Line)
		if err != nil {
			if ctx.Err() != nil {
				r.reject("canceled")
			} else {
				r.reject("source_read_failed")
			}
			return r
		}
		if !complete {
			if len(line) > 0 || oversized {
				r.gap("incomplete_tail")
			}
			break
		}
		if oversized {
			r.gap("record_limit")
			currentTurn, currentTurnKey = -1, ""
			continue
		}
		r.AnalyzedBytes += int64(len(line))
		if exceedsDepth(line, limits.Depth) {
			r.gap("parse_limit")
			currentTurn, currentTurnKey = -1, ""
			continue
		}
		if !utf8.Valid(line) {
			r.gap("invalid_record")
			currentTurn, currentTurnKey = -1, ""
			continue
		}
		var record map[string]any
		if json.Unmarshal(line, &record) != nil || record == nil {
			r.gap("invalid_record")
			currentTurn, currentTurnKey = -1, ""
			continue
		}
		kind, ok := record["type"].(string)
		p, payloadOK := record["payload"].(map[string]any)
		if !ok || kind == "" || !payloadOK {
			r.gap("missing_key")
			currentTurn, currentTurnKey = -1, ""
			continue
		}
		for key := range record {
			switch key {
			case "type", "payload", "timestamp", "ordinal", "metadata":
			default:
				r.gap("unknown_fields")
			}
		}
		// Only understood content-bearing payloads have a field vocabulary.
		// Metadata events and embedded tool schemas keep their own namespaces;
		// unknown payload content is never guessed into other_observed.
		known := ""
		switch kind {
		case "session_meta":
			known = " id timestamp cwd originator cli_version source model_provider base_instructions dynamic_tools git forked_from_id parent_thread_id agent_path subagent_history_start_ordinal agent_nickname agent_role "
		case "turn_context":
			known = " turn_id cwd model approval_policy sandbox_policy effort summary personality collaboration_mode truncation_policy "
		case "response_item":
			known = " type id role content output summary encrypted_content arguments input tools name call_id status phase end_turn namespace truncated output_truncated "
		case "dynamic_tools_updated":
			known = " thread_id tools "
		case "compacted":
			known = " message "
		}
		if known != "" {
			fields := strings.Fields(known)
			for key := range p {
				recognized := false
				for _, field := range fields {
					if key == field {
						recognized = true
						break
					}
				}
				if !recognized {
					r.gap("unknown_fields")
				}
			}
		}
		if kind == "session_meta" {
			if stringValue(p["id"]) != nativeID {
				r.reject("source_identity_mismatch")
				return r
			}
			if stringValue(p["cli_version"]) != sourceVersion {
				r.reject("unsupported_source_version")
				return r
			}
			source, isString := p["source"].(string)
			if !isString || (source != "cli" && source != "exec" && source != "vscode") {
				r.reject("subagent_source")
				return r
			}
			if p["parent_thread_id"] != nil || p["agent_path"] != nil || p["subagent_history_start_ordinal"] != nil {
				r.reject("subagent_source")
				return r
			}
			if verified {
				r.reject("source_identity_mismatch")
				return r
			}
			verified, r.SourceVersion = true, sourceVersion
			if p["forked_from_id"] != nil {
				r.gap("fork_context_unknown")
			}
			r.add(0, textBytes(p["base_instructions"]), -1)
			r.add(1, schemaBytes(line, true), -1)
			continue
		}
		if !verified {
			continue
		}
		r.Records++
		switch kind {
		case "turn_context":
			key := stringValue(p["turn_id"])
			if key == "" {
				currentTurn, currentTurnKey = -1, ""
				r.gap("missing_key")
				continue
			}
			if key != currentTurnKey || currentTurn < 0 {
				// Numeric projection has the existing 16 MiB/session ceiling.
				// Conservative per-row reservation also bounds in-memory numbers.
				if len(r.Turns) >= (16<<20)/1024 {
					r.gap("projection_limit")
					currentTurn, currentTurnKey = -1, ""
					continue
				}
				currentTurn = len(r.Turns)
				currentTurnKey = key
				r.Turns = append(r.Turns, recordedTurn{Ordinal: int64(currentTurn + 1), Buckets: emptyBuckets()})
			}
		case "response_item":
			r.item(p, line, currentTurn)
		case "compacted":
			r.gap("compaction_gap")
			currentTurn, currentTurnKey = -1, ""
			r.add(2, textBytes(p["message"]), -1)
		case "event_msg":
			// Native event messages duplicate response items or describe usage;
			// neither becomes a second content or canonical ledger entry.
			if event := stringValue(p["type"]); event == "task_complete" || event == "turn_aborted" || event == "task_started" {
				currentTurn, currentTurnKey = -1, ""
			}
		case "dynamic_tools_updated":
			if stringValue(p["thread_id"]) != nativeID {
				r.reject("source_identity_mismatch")
				return r
			}
			r.add(1, schemaBytes(line, false), currentTurn)
		case "retained_context":
			r.gap("retained_context_unknown")
		case "token_usage_record", "world_state", "security_risk_score":
			// Metadata is not additional observed model input.
		default:
			r.gap("unknown_schema")
			currentTurn, currentTurnKey = -1, ""
		}
	}
	if !verified {
		r.reject("source_version_unknown")
	}
	return r
}

func (r *recording) item(p map[string]any, line []byte, turn int) {
	for _, field := range []string{"content", "output", "summary"} {
		if p[field] != nil && unsupportedText(p[field]) {
			r.gap("unsupported_content")
		}
	}
	switch stringValue(p["type"]) {
	case "message":
		category := 2
		switch stringValue(p["role"]) {
		case "system", "developer":
			category = 0
		case "user", "assistant":
		default:
			category = 5
			r.gap("unknown_schema")
		}
		r.add(category, textBytes(p["content"]), turn)
	case "function_call_output", "custom_tool_call_output":
		r.add(3, textBytes(p["output"]), turn)
	case "additional_tools":
		r.add(1, schemaBytes(line, false), turn)
	case "reasoning":
		r.add(4, textBytes(p["summary"]), turn)
		r.add(4, textBytes(p["content"]), turn)
		if p["encrypted_content"] != nil {
			r.gap("hidden_thinking")
		}
	case "function_call":
		r.add(5, textBytes(p["arguments"]), turn)
	case "custom_tool_call":
		r.add(5, textBytes(p["input"]), turn)
	default:
		r.gap("unknown_schema")
	}
	if p["truncated"] == true || p["output_truncated"] == true {
		r.gap("source_truncated")
	}
	if text, ok := p["output"].(string); ok && strings.Contains(text, "... command output truncated for persistence ...") {
		r.gap("source_truncated")
	}
}

func stringValue(v any) string { s, _ := v.(string); return s }

func textBytes(v any) int64 {
	switch v := v.(type) {
	case string:
		return int64(len(v))
	case []any:
		var n int64
		found := len(v) == 0
		for _, item := range v {
			if value := textBytes(item); value >= 0 {
				n += value
				found = true
			}
		}
		if !found {
			return -1
		}
		return n
	case map[string]any:
		if kind := stringValue(v["type"]); kind != "" {
			switch kind {
			case "input_text", "output_text", "text", "summary_text", "reasoning_text":
			default:
				return -1
			}
		}
		// Only content fields; IDs, names, paths and encrypted material are
		// never interpreted as visible text or retained in the projection.
		var n int64
		found := false
		for _, key := range []string{"text", "content"} {
			if value := textBytes(v[key]); value >= 0 {
				n += value
				found = true
			}
		}
		if !found {
			return -1
		}
		return n
	}
	return -1
}

func schemaBytes(line []byte, dynamic bool) int64 {
	// Only this field's necessary decode view is copied; do not reserialize the
	// schema (HTML escaping and whitespace normalization change its byte count).
	var value struct {
		Payload struct {
			Dynamic json.RawMessage `json:"dynamic_tools"`
			Tools   json.RawMessage `json:"tools"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &value) != nil {
		return -1
	}
	b := value.Payload.Tools
	if dynamic {
		b = value.Payload.Dynamic
	}
	if len(b) == 0 || bytes.Equal(b, []byte("null")) || b[0] != '[' {
		return -1
	}
	return int64(len(b))
}

func unsupportedText(v any) bool {
	switch v := v.(type) {
	case string:
		return false
	case []any:
		for _, item := range v {
			if unsupportedText(item) {
				return true
			}
		}
		return false
	case map[string]any:
		if textBytes(v) < 0 {
			return true
		}
		for _, key := range []string{"text", "content"} {
			if child, ok := v[key]; ok && unsupportedText(child) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func exceedsDepth(b []byte, maxDepth int) bool {
	depth, inString, escape := 0, false, false
	for _, c := range b {
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
		case '[', '{':
			depth++
			if depth > maxDepth {
				return true
			}
		case ']', '}':
			depth--
		}
	}
	return false
}

func readLine(ctx context.Context, r *bufio.Reader, limit int64) ([]byte, bool, bool, error) {
	var line []byte
	var size int64
	oversized := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, false, err
		}
		part, err := r.ReadSlice('\n')
		size += int64(len(part))
		if size > limit {
			oversized, line = true, nil
		}
		if !oversized {
			line = append(line, part...)
		}
		switch {
		case err == nil:
			return line, oversized, true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line, oversized, false, nil
		default:
			return nil, false, false, err
		}
	}
}
