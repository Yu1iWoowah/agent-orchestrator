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
	summary, _ := json.Marshal(map[string]any{"uuid": "summary", "parentUuid": "a1", "sessionId": old, "type": "user", "isSidechain": false, "isCompactSummary": true, "version": "2.1.0", "message": map[string]any{"content": "provider compaction summary"}})
	ancestor += string(summary) + "\n"
	current := ancestor + makeRows(next, "u2", "a2", "summary", "new question")
	for id, data := range map[string]string{old: ancestor, next: current} {
		if e := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	s := domain.TranscriptSourceSnapshot{Session: domain.SessionRecord{ID: "source", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode}, Fingerprint: "fixed", NativeSessions: []domain.TranscriptNativeSession{{Harness: domain.HarnessClaudeCode, NativeID: old, ConfigDir: root}, {Harness: domain.HarnessClaudeCode, NativeID: next, ConfigDir: root}}}
	cuts := []EpochCutoff{{NativeID: old, Bytes: int64(len(ancestor)), LeafID: "summary", Schema: ports.ClaudeTranscriptSchemaV1}, {NativeID: next, Bytes: int64(len(current)), LeafID: "a2", Schema: ports.ClaudeTranscriptSchemaV1}}
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

func TestTranscriptNativeBatchAndSteerRetainSenderAndAttachmentReferences(t *testing.T) {
	root := t.TempDir()
	id := "11111111-1111-4111-8111-111111111111"
	dir := filepath.Join(root, "projects/workspace")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	combined := "AO pending messages in enqueue order. Each JSON item is one complete message; kind steer identifies active-turn guidance. Content blocks follow the same item order, using contentStart and contentCount indices.\n" +
		`[{"kind":"message","text":"first","origin":"human","senderSessionId":"sender-one","contentCount":1},{"kind":"message","text":"second","origin":"automation","senderSessionId":"sender-two","contentStart":1}]`
	rows := []map[string]any{
		{"uuid": "u", "parentUuid": "", "type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": combined}, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "fixture"}}}}},
		{"uuid": "q", "parentUuid": "u", "type": "attachment", "attachment": map[string]any{"type": "queued_command", "prompt": "steered third message"}},
		{"uuid": "a", "parentUuid": "q", "type": "assistant", "message": map[string]any{"content": "combined answer", "stop_reason": "end_turn"}},
	}
	var body strings.Builder
	for _, row := range rows {
		row["sessionId"], row["isSidechain"], row["version"], row["promptId"] = id, false, "2.1.0", "native-prompt"
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(data)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	detail, err := json.Marshal(map[string]any{"event": "steer", "providerInputCaptured": true, "providerInputText": "steered third message", "providerInputSourceTurnIds": []string{"steered"}})
	if err != nil {
		t.Fatal(err)
	}
	s := domain.TranscriptSourceSnapshot{
		Session: domain.SessionRecord{ID: "source", ProjectID: "project", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode}, Fingerprint: "fixed",
		NativeSessions: []domain.TranscriptNativeSession{{Harness: domain.HarnessClaudeCode, NativeID: id, ConfigDir: root}},
		Branches:       []domain.ConversationBranch{{ID: "b", ProviderConversationID: id}},
		NativeThreads:  []domain.ConversationNativeThreadEvidence{{BranchID: "b", ProviderTurnID: "opaque", ProviderConversationID: id}},
		Turns:          []domain.ConversationTurn{{ID: "lead", BranchID: "b", ProviderTurnID: "opaque", State: domain.TurnStateCompleted}, {ID: "second", BranchID: "b", State: domain.TurnStateCompleted}, {ID: "steered", BranchID: "b", State: domain.TurnStateCompleted}},
		Inputs:         []domain.TranscriptTurnInput{{TurnID: "lead", ProviderInputText: combined}, {TurnID: "second", PromotedToTurnID: "lead", BatchedInput: true}, {TurnID: "steered", PromotedToTurnID: "lead", BatchedInput: true}},
		Activities:     []domain.ConversationActivity{{ID: "steer-receipt", TurnID: "lead", Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted, Detail: detail}},
	}
	out, err := Build(context.Background(), s, []EpochCutoff{{NativeID: id, Bytes: int64(body.Len()), LeafID: "a", Schema: ports.ClaudeTranscriptSchemaV1}}, claudecode.New())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Manifest.CoveredTurns) != 3 || len(out.Manifest.Steers) != 1 || out.Manifest.Segments[0].Counts.Attachments != 1 {
		t.Fatal("lost member, steer or attachment coverage")
	}
	for _, text := range []string{"sender-one", "sender-two", "steered third message", "image sha256:"} {
		if strings.Count(string(out.Transcript), text) != 1 {
			t.Fatalf("missing or duplicated native source %q", text)
		}
	}
}
