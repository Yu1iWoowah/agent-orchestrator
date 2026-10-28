package transcriptexport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestTranscriptFullNativeBeyondTUIHistory(t *testing.T) {
	root := t.TempDir()
	id := "11111111-1111-4111-8111-111111111111"
	dir := filepath.Join(root, "projects/workspace")
	if e := os.MkdirAll(dir, 0o700); e != nil {
		t.Fatal(e)
	}
	var b strings.Builder
	parent := ""
	var terminal []domain.TranscriptTerminalTurn
	for i := 0; i < 60; i++ {
		u := fmt.Sprint("u", i)
		a := fmt.Sprint("a", i)
		question := fmt.Sprint("question-", i)
		answer := fmt.Sprint("answer-", i)
		for _, r := range []map[string]any{{"uuid": u, "parentUuid": parent, "type": "user", "message": map[string]any{"content": question}}, {"uuid": a, "parentUuid": u, "type": "assistant", "message": map[string]any{"content": answer, "stop_reason": "end_turn"}}} {
			r["sessionId"] = id
			r["isSidechain"] = false
			r["version"] = "2.1.0"
			r["promptId"] = fmt.Sprint("p", i)
			data, e := json.Marshal(r)
			if e != nil {
				t.Fatal(e)
			}
			b.Write(data)
			b.WriteByte('\n')
		}
		parent = a
		if i >= 10 {
			terminal = append(terminal, domain.TranscriptTerminalTurn{ID: int64(i), Harness: domain.HarnessClaudeCode, NativeID: id, Request: question, Response: answer})
		}
	}
	if e := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(b.String()), 0o600); e != nil {
		t.Fatal(e)
	}
	source := domain.TranscriptSourceSnapshot{Session: domain.SessionRecord{ID: "source", ProjectID: "project", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeTUI}, Fingerprint: "fixed", Terminal: terminal, NativeSessions: []domain.TranscriptNativeSession{{Harness: domain.HarnessClaudeCode, NativeID: id, ConfigDir: root}}}
	out, e := Build(context.Background(), source, []EpochCutoff{{NativeID: id, Bytes: int64(b.Len()), LeafID: parent, Schema: ports.ClaudeTranscriptSchemaV1}}, claudecode.New())
	if e != nil {
		t.Fatal(e)
	}
	if out.Manifest.Segments[0].Counts.UserRecords != 60 || !strings.Contains(string(out.Transcript), "question-0") || !strings.Contains(string(out.Transcript), "answer-59") {
		t.Fatal("export used the retained summary instead of native full history")
	}
	if e := out.Revalidate(context.Background()); e != nil {
		t.Fatal(e)
	}
	f, e := os.OpenFile(filepath.Join(dir, id+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.WriteString("{}\n")
	_ = f.Close()
	if e = out.Revalidate(context.Background()); e == nil {
		t.Fatal("source mutation after Build accepted")
	}
}

func TestTranscriptVerifiedParentEpochAncestry(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects/workspace")
	if e := os.MkdirAll(dir, 0o700); e != nil {
		t.Fatal(e)
	}
	old := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	next := "11111111-1111-4111-8111-111111111111"
	makeRows := func(native, u, a, parent, text string) string {
		var b strings.Builder
		for _, r := range []map[string]any{{"uuid": u, "parentUuid": parent, "type": "user", "message": map[string]any{"content": text}}, {"uuid": a, "parentUuid": u, "type": "assistant", "message": map[string]any{"content": text + " answer", "stop_reason": "end_turn"}}} {
			r["sessionId"] = native
			r["isSidechain"] = false
			r["version"] = "2.1.0"
			r["promptId"] = u
			raw, e := json.Marshal(r)
			if e != nil {
				t.Fatal(e)
			}
			b.Write(raw)
			b.WriteByte('\n')
		}
		return b.String()
	}
	ancestor := makeRows(old, "u1", "a1", "", "old question")
	current := ancestor + makeRows(next, "u2", "a2", "a1", "new question")
	for id, data := range map[string]string{old: ancestor, next: current} {
		if e := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	s := domain.TranscriptSourceSnapshot{Session: domain.SessionRecord{ID: "source", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode}, Fingerprint: "fixed", NativeSessions: []domain.TranscriptNativeSession{{Harness: domain.HarnessClaudeCode, NativeID: old, ConfigDir: root}, {Harness: domain.HarnessClaudeCode, NativeID: next, ConfigDir: root}}}
	cuts := []EpochCutoff{{NativeID: old, Bytes: int64(len(ancestor)), LeafID: "a1", Schema: ports.ClaudeTranscriptSchemaV1}, {NativeID: next, Bytes: int64(len(current)), LeafID: "a2", Schema: ports.ClaudeTranscriptSchemaV1}}
	out, e := Build(context.Background(), s, cuts, claudecode.New())
	if e != nil {
		t.Fatal(e)
	}
	if strings.Count(string(out.Transcript), "old question answer") != 1 || strings.Count(string(out.Transcript), "new question answer") != 1 {
		t.Fatal("ancestor duplicated or new content lost")
	}
	if e := out.Revalidate(context.Background()); e != nil {
		t.Fatal(e)
	}
	// Same UUID and foreign native id with changed content is not verified ancestry.
	changed := strings.Replace(current, "old question answer", "CHANGED", 1)
	if e := os.WriteFile(filepath.Join(dir, next+".jsonl"), []byte(changed), 0o600); e != nil {
		t.Fatal(e)
	}
	cuts[1].Bytes = int64(len(changed))
	if _, e = Build(context.Background(), s, cuts, claudecode.New()); e == nil {
		t.Fatal("conflicting ancestor record accepted")
	}
}
