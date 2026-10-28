package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const exportNativeID = "11111111-1111-4111-8111-111111111111"

func transcriptFixture(t *testing.T, rows []map[string]any) ports.NativeTranscriptRequest {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "workspace")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, row := range rows {
		if _, ok := row["sessionId"]; !ok {
			row["sessionId"] = exportNativeID
		}
		if _, ok := row["version"]; !ok && row["type"] != "queue-operation" {
			row["version"] = "2.1.0"
		}
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, exportNativeID+".jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return ports.NativeTranscriptRequest{Source: ports.NativeSessionRef{NativeSessionID: exportNativeID, ConfigDir: root}, Schema: ports.ClaudeTranscriptSchemaV1, ByteCutoff: int64(b.Len()), LeafID: "a"}
}

func TestClaudeTranscriptAOHookAndQueueMetadata(t *testing.T) {
	start := mainRow("start", "", "progress", nil)
	start["data"] = map[string]any{"type": "hook_progress", "hookEvent": "SessionStart"}
	submit := mainRow("submit", "u", "progress", nil)
	submit["data"] = map[string]any{"type": "hook_progress", "hookEvent": "UserPromptSubmit"}
	post := mainRow("post", "result", "progress", nil)
	post["data"] = map[string]any{"type": "hook_progress", "hookEvent": "PostToolUse"}
	queued := mainRow("q", "post", "attachment", nil)
	queued["attachment"] = map[string]any{"type": "queued_command", "prompt": "queued human text"}
	local := mainRow("local", "q", "system", nil)
	local["subtype"] = "local_command"
	stop := mainRow("stop", "a", "system", nil)
	stop["subtype"] = "stop_hook_summary"
	rows := []map[string]any{start, mainRow("u", "start", "user", "original human"), submit,
		mainRow("call", "submit", "assistant", []any{map[string]any{"type": "tool_use", "id": "read", "name": "Read", "input": map[string]any{"file_path": "fixture.txt"}}}),
		mainRow("result", "call", "user", []any{map[string]any{"type": "tool_result", "tool_use_id": "read", "content": "result"}}), post,
		{"type": "queue-operation", "operation": "enqueue", "content": "queue metadata payload"}, queued,
		{"type": "queue-operation", "operation": "dequeue"}, local, mainRow("a", "local", "assistant", "final answer"), stop}
	request := transcriptFixture(t, rows)
	request.LeafID = "stop"
	segment, err := New().ReadNativeTranscript(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if segment.Counts.MetadataRecords != 7 || segment.Counts.MainRecords != 5 || len(segment.Records) != 12 || segment.TailState != "complete" {
		t.Fatalf("hook/queue ledger or tail incorrect: %+v", segment.Counts)
	}
	for _, r := range segment.Records {
		if r.Type == "progress" || r.Type == "system" || r.Type == "queue-operation" {
			if r.Excluded != "metadata" || r.RecordHash == "" || r.Provenance != domain.NativeThreadMain || len(r.Blocks) != 0 {
				t.Fatalf("metadata not safely retained: %+v", r)
			}
		}
	}
}

func TestClaudeTranscriptKnownMetadataEnvelopesAndUnknownForms(t *testing.T) {
	for _, tc := range []struct{ name, kind, subtype, operation string }{
		{"stop", "system", "stop_hook_summary", ""}, {"local", "system", "local_command", ""},
		{"hook", "progress", "hook_progress", ""}, {"enqueue", "queue-operation", "", "enqueue"}, {"dequeue", "queue-operation", "", "dequeue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := mainRow("meta", "u", tc.kind, nil)
			meta["subtype"], meta["operation"] = tc.subtype, tc.operation
			if tc.kind == "progress" {
				meta["data"] = map[string]any{"type": tc.subtype}
			}
			if tc.kind == "queue-operation" {
				delete(meta, "isSidechain")
			}
			r := transcriptFixture(t, []map[string]any{mainRow("u", "", "user", "original"), meta, mainRow("a", "meta", "assistant", "answer")})
			if s, err := New().ReadNativeTranscript(context.Background(), r); err != nil || s.Counts.MetadataRecords != 1 || s.TailState != "complete" {
				t.Fatalf("known metadata rejected: %+v %v", s.Counts, err)
			}
		})
	}
	for _, meta := range []map[string]any{
		{"type": "system", "subtype": "future_system", "isSidechain": false},
		{"type": "progress", "data": map[string]any{"type": "future_progress"}, "isSidechain": false},
		{"type": "queue-operation", "operation": "future_queue"},
		{"type": "queue-operation", "operation": "enqueue", "sessionId": "foreign"},
		{"type": "system", "subtype": "stop_hook_summary", "sessionId": "", "isSidechain": false},
		{"type": "progress", "data": map[string]any{"type": "hook_progress"}, "sessionId": "", "isSidechain": false},
		{"type": "file-history-snapshot", "sessionId": "", "isSidechain": true},
	} {
		r := transcriptFixture(t, []map[string]any{mainRow("u", "", "user", "original"), meta, mainRow("a", "u", "assistant", "answer")})
		if _, err := New().ReadNativeTranscript(context.Background(), r); err == nil {
			t.Fatal("unknown metadata accepted", meta)
		}
	}
}

func TestClaudeTranscriptPreventedStopContinuationIsUnsettled(t *testing.T) {
	stop := mainRow("stop", "a", "system", nil)
	stop["subtype"], stop["preventedContinuation"] = "stop_hook_summary", true
	r := transcriptFixture(t, []map[string]any{mainRow("u", "", "user", "original"), mainRow("a", "u", "assistant", "answer"), stop})
	r.LeafID = "stop"
	_, err := New().ReadNativeTranscript(context.Background(), r)
	var failure *ports.NativeTranscriptError
	if !errors.As(err, &failure) || failure.Code != "TRANSCRIPT_TAIL_UNSETTLED" {
		t.Fatalf("prevented continuation not unsettled: %v", err)
	}
	r.LeafID = "a"
	if _, err = New().ReadNativeTranscript(context.Background(), r); !errors.As(err, &failure) || failure.Code != "TRANSCRIPT_TAIL_UNSETTLED" {
		t.Fatalf("pending hook after selected reply ignored: %v", err)
	}
	r = transcriptFixture(t, []map[string]any{mainRow("u", "", "user", "original"), mainRow("a", "u", "assistant", "first answer"), stop, mainRow("later", "a", "assistant", "settled continuation")})
	r.LeafID = "later"
	if segment, err := New().ReadNativeTranscript(context.Background(), r); err != nil || segment.TailState != "complete" {
		t.Fatalf("historical pending hook blocked later completion: %v", err)
	}
	delete(stop, "uuid")
	r = transcriptFixture(t, []map[string]any{mainRow("u", "", "user", "original"), mainRow("a", "u", "assistant", "answer"), stop})
	if _, err = New().ReadNativeTranscript(context.Background(), r); err == nil {
		t.Fatal("pending continuation with no lineage identity was ignored")
	}
}
func mainRow(id, parent, kind string, content any) map[string]any {
	return map[string]any{"uuid": id, "parentUuid": parent, "type": kind, "isSidechain": false, "promptId": "prompt", "message": map[string]any{"content": content, "stop_reason": "end_turn"}}
}
func TestClaudeTranscriptExportCoverage(t *testing.T) {
	user := mainRow("u", "", "user", "same text")
	child := mainRow("u", "", "user", "same text")
	child["isSidechain"] = true
	child["agentId"] = "child"
	request := transcriptFixture(t, []map[string]any{
		user, child,
		mainRow("mid", "u", "assistant", []any{map[string]any{"type": "text", "text": "intermediate"}, map[string]any{"type": "tool_use", "id": "tool", "name": "Read", "input": map[string]any{"file_path": "a.txt"}}, map[string]any{"type": "tool_use", "id": "tool-2", "name": "Bash", "input": map[string]any{"command": "false"}}}),
		mainRow("results", "mid", "user", []any{map[string]any{"type": "tool_result", "tool_use_id": "tool", "content": "large output"}, map[string]any{"type": "tool_result", "tool_use_id": "tool-2", "content": "other output", "is_error": true}}),
		mainRow("a", "results", "assistant", "final"),
	})
	segment, err := New().ReadNativeTranscript(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if segment.Counts.ChildRecords != 1 || segment.Counts.MainRecords != 4 || segment.Counts.ToolCalls != 2 || segment.Counts.ToolResults != 2 {
		t.Fatalf("counts=%+v", segment.Counts)
	}
	if len(segment.Records) != 5 || segment.Records[1].Provenance != domain.NativeThreadChild {
		t.Fatalf("records=%+v", segment.Records)
	}
	if segment.Records[2].Blocks[0].Text != "intermediate" || segment.Records[4].Blocks[0].Text != "final" {
		t.Fatal("lost readable text")
	}
	if segment.SHA256 == "" || segment.ByteCutoff != request.ByteCutoff {
		t.Fatal("missing source snapshot evidence")
	}
}

func TestClaudeTranscriptExportRefusesUnverifiedRecords(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		mutate     func([]map[string]any, *ports.NativeTranscriptRequest)
	}{
		{"missing provenance", "TRANSCRIPT_PROVENANCE_UNKNOWN", func(rows []map[string]any, _ *ports.NativeTranscriptRequest) { delete(rows[0], "isSidechain") }},
		{"conflicting provenance", "TRANSCRIPT_PROVENANCE_UNKNOWN", func(rows []map[string]any, _ *ports.NativeTranscriptRequest) { rows[0]["agentId"] = "child" }},
		{"foreign native", "TRANSCRIPT_PROVENANCE_UNKNOWN", func(rows []map[string]any, _ *ports.NativeTranscriptRequest) {
			rows[0]["sessionId"] = "22222222-2222-4222-8222-222222222222"
		}},
		{"unknown type", "TRANSCRIPT_SCHEMA_UNSUPPORTED", func(rows []map[string]any, _ *ports.NativeTranscriptRequest) { rows[0]["type"] = "future-user" }},
		{"unknown version", "TRANSCRIPT_SCHEMA_UNSUPPORTED", func(rows []map[string]any, _ *ports.NativeTranscriptRequest) { rows[0]["version"] = "99.0.0" }},
		{"malformed version", "TRANSCRIPT_SCHEMA_UNSUPPORTED", func(rows []map[string]any, _ *ports.NativeTranscriptRequest) { rows[0]["version"] = "2.1.bad" }},
		{"child unknown version", "TRANSCRIPT_SCHEMA_UNSUPPORTED", func(rows []map[string]any, _ *ports.NativeTranscriptRequest) {
			rows[0]["isSidechain"] = true
			rows[0]["version"] = "99.0.0"
		}},
		{"broken ancestry", "TRANSCRIPT_ANCESTRY_INCOMPLETE", func(rows []map[string]any, _ *ports.NativeTranscriptRequest) { rows[1]["parentUuid"] = "missing" }},
		{"compact boundary", "TRANSCRIPT_ANCESTRY_INCOMPLETE", func(rows []map[string]any, _ *ports.NativeTranscriptRequest) {
			rows[0]["type"] = "system"
			rows[0]["subtype"] = "compact_boundary"
		}},
		{"conflicting UUID", "TRANSCRIPT_IDENTITY_CONFLICT", func(rows []map[string]any, _ *ports.NativeTranscriptRequest) { rows[1]["uuid"] = "u" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []map[string]any{mainRow("u", "", "user", "prompt"), mainRow("a", "u", "assistant", "reply")}
			tc.mutate(rows, nil)
			request := transcriptFixture(t, rows)
			_, err := New().ReadNativeTranscript(context.Background(), request)
			var failure *ports.NativeTranscriptError
			if !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
		})
	}
}
func TestClaudeTranscriptCutoffIsExplicit(t *testing.T) {
	request := transcriptFixture(t, []map[string]any{mainRow("u", "", "user", "prompt"), mainRow("a", "u", "assistant", "reply")})
	request.ByteCutoff--
	_, err := New().ReadNativeTranscript(context.Background(), request)
	var failure *ports.NativeTranscriptError
	if !errors.As(err, &failure) || failure.Code != "TRANSCRIPT_CUTOFF_INVALID" {
		t.Fatalf("partial cutoff accepted: %v", err)
	}
}

func TestClaudeTranscriptLedgerUsesAncestryRatherThanFileOrder(t *testing.T) {
	u := mainRow("u", "", "user", "same")
	request := transcriptFixture(t, []map[string]any{
		{"type": "file-history-snapshot", "sessionId": ""},
		u, mainRow("a", "mid", "assistant", "same"), mainRow("mid", "u", "assistant", "intermediate"), u,
	})
	segment, err := New().ReadNativeTranscript(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(segment.MainRecordIDs, ",") != "u,mid,a" {
		t.Fatalf("main order=%v", segment.MainRecordIDs)
	}
	if segment.Counts.Duplicates != 1 || segment.Counts.MetadataRecords != 1 || segment.Counts.AssistantRecords != 2 {
		t.Fatalf("counts=%+v", segment.Counts)
	}
}

func TestClaudeTranscriptIOBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(*testing.T, *ports.NativeTranscriptRequest)
	}{
		{"record limit", "TRANSCRIPT_LIMIT_EXCEEDED", func(_ *testing.T, r *ports.NativeTranscriptRequest) { r.MaxRecordBytes = 32 }},
		{"source limit", "TRANSCRIPT_LIMIT_EXCEEDED", func(_ *testing.T, r *ports.NativeTranscriptRequest) { r.MaxSourceBytes = 1 }},
		{"count limit", "TRANSCRIPT_LIMIT_EXCEEDED", func(_ *testing.T, r *ports.NativeTranscriptRequest) { r.MaxRecords = 1 }},
		{"ambiguous file", "TRANSCRIPT_SOURCE_AMBIGUOUS", func(t *testing.T, r *ports.NativeTranscriptRequest) {
			original := filepath.Join(r.Source.ConfigDir, "projects", "workspace", exportNativeID+".jsonl")
			b, err := os.ReadFile(original)
			if err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(r.Source.ConfigDir, "projects", "other")
			if err := os.Mkdir(other, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, exportNativeID+".jsonl"), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing file", "TRANSCRIPT_SOURCE_MISSING", func(t *testing.T, r *ports.NativeTranscriptRequest) {
			if err := os.Remove(filepath.Join(r.Source.ConfigDir, "projects", "workspace", exportNativeID+".jsonl")); err != nil {
				t.Fatal(err)
			}
		}},
		{"symbolic link", "TRANSCRIPT_SOURCE_INVALID", func(t *testing.T, r *ports.NativeTranscriptRequest) {
			p := filepath.Join(r.Source.ConfigDir, "projects", "workspace", exportNativeID+".jsonl")
			if err := os.Rename(p, p+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(p+".real", p); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := transcriptFixture(t, []map[string]any{mainRow("u", "", "user", "prompt"), mainRow("a", "u", "assistant", "reply")})
			tc.change(t, &r)
			_, err := New().ReadNativeTranscript(context.Background(), r)
			var failure *ports.NativeTranscriptError
			if !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
		})
	}
}
func TestClaudeTranscriptReadCancellation(t *testing.T) {
	r := transcriptFixture(t, []map[string]any{mainRow("a", "", "assistant", "reply")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().ReadNativeTranscript(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestClaudeTranscriptRejectsUnsettledOrAmbiguousPayload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]map[string]any)
		raw    func(string) string
	}{
		{name: "pending assistant", mutate: func(rows []map[string]any) { rows[1]["message"].(map[string]any)["stop_reason"] = nil }},
		{name: "orphan result", mutate: func(rows []map[string]any) {
			rows[0]["message"].(map[string]any)["content"] = []any{map[string]any{"type": "tool_result", "tool_use_id": "missing"}}
		}},
		{name: "duplicate sidechain key", raw: func(s string) string {
			return strings.Replace(s, `"isSidechain":false`, `"isSidechain":true,"isSidechain":false`, 1)
		}},
		{name: "unpaired Unicode", raw: func(s string) string { return strings.Replace(s, `"content":"final"`, `"content":"\ud800"`, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []map[string]any{mainRow("u", "", "user", "question"), mainRow("a", "u", "assistant", "final")}
			if tc.mutate != nil {
				tc.mutate(rows)
			}
			r := transcriptFixture(t, rows)
			if tc.raw != nil {
				p := filepath.Join(r.Source.ConfigDir, "projects", "workspace", exportNativeID+".jsonl")
				b, e := os.ReadFile(p)
				if e != nil {
					t.Fatal(e)
				}
				b = []byte(tc.raw(string(b)))
				if e := os.WriteFile(p, b, 0o600); e != nil {
					t.Fatal(e)
				}
				r.ByteCutoff = int64(len(b))
			}
			if _, e := New().ReadNativeTranscript(context.Background(), r); e == nil {
				t.Fatal("unsettled or ambiguous record accepted")
			}
		})
	}
}

func TestClaudeTranscriptQueuedCompanionAndInterrupted(t *testing.T) {
	q := mainRow("q", "u", "attachment", nil)
	q["attachment"] = map[string]any{"type": "queued_command", "prompt": "queued original"}
	companion := mainRow("c", "q", "user", "companion context")
	companion["isMeta"] = true
	companion["turnCompanion"] = true
	request := transcriptFixture(t, []map[string]any{mainRow("u", "", "user", "request"), q, companion, mainRow("tool", "c", "assistant", []any{map[string]any{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]any{"command": "sleep 1"}}}), mainRow("a", "tool", "user", []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}})})
	segment, e := New().ReadNativeTranscript(context.Background(), request)
	if e != nil {
		t.Fatal(e)
	}
	if segment.TailState != "interrupted" || segment.Counts.UnresolvedToolCalls != 1 || segment.Records[1].Blocks[0].Text != "queued original" || !segment.Records[2].Companion {
		t.Fatalf("lost queued/interrupted provenance: %+v", segment)
	}
}
func TestClaudeTranscriptExceedsTerminalSummaryRetention(t *testing.T) {
	var rows []map[string]any
	parent := ""
	for i := 0; i < 60; i++ {
		user := fmt.Sprint("u", i)
		assistant := fmt.Sprint("a", i)
		if i == 59 {
			assistant = "a"
		}
		u := mainRow(user, parent, "user", fmt.Sprint("request-", i))
		u["promptId"] = fmt.Sprint("prompt-", i)
		a := mainRow(assistant, user, "assistant", fmt.Sprint("answer-", i))
		a["promptId"] = fmt.Sprint("prompt-", i)
		rows = append(rows, u, a)
		parent = assistant
	}
	r := transcriptFixture(t, rows)
	segment, e := New().ReadNativeTranscript(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	if segment.Counts.UserRecords != 60 || segment.Counts.AssistantRecords != 60 || len(segment.MainRecordIDs) != 120 || segment.Records[0].Blocks[0].Text != "request-0" {
		t.Fatal("native history was reduced to summary retention")
	}
}

func TestClaudeTranscriptCompactionSummaryIsNotHumanInput(t *testing.T) {
	summary := mainRow("summary", "a1", "user", "This session is being continued from a previous conversation that ran out of context. Summary: prior work.")
	summary["isCompactSummary"] = true
	request := transcriptFixture(t, []map[string]any{mainRow("u1", "", "user", "original human"), mainRow("a1", "u1", "assistant", "original answer"), summary, mainRow("u2", "summary", "user", "new human"), mainRow("a", "u2", "assistant", "new answer")})
	segment, e := New().ReadNativeTranscript(context.Background(), request)
	if e != nil {
		t.Fatal(e)
	}
	if segment.Counts.UserRecords != 2 || segment.Counts.MetadataRecords != 1 || segment.Records[2].Excluded != "compaction-summary" {
		t.Fatal("provider summary became human input")
	}
	// Prefix alone does not classify a real user's text as provider metadata.
	human := mainRow("u", "", "user", "This session is being continued from a previous conversation that ran out of context. I am quoting this sentence.")
	request = transcriptFixture(t, []map[string]any{human, mainRow("a", "u", "assistant", "answer")})
	segment, e = New().ReadNativeTranscript(context.Background(), request)
	if e != nil || segment.Counts.UserRecords != 1 {
		t.Fatal("human prefix dropped", e)
	}
	summary["parentUuid"] = ""
	request = transcriptFixture(t, []map[string]any{summary, mainRow("a", "summary", "assistant", "after summary")})
	if _, e = New().ReadNativeTranscript(context.Background(), request); e == nil {
		t.Fatal("summary substituted for missing original ancestry")
	}
}
