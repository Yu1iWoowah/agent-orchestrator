package transcriptexport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func writeCaptureEpoch(t *testing.T, root, native, prefix, parent string, child bool) int64 {
	t.Helper()
	rows := []map[string]any{{"uuid": prefix + "-u", "parentUuid": parent, "type": "user", "message": map[string]any{"content": prefix + " prompt"}}, {"uuid": prefix + "-a", "parentUuid": prefix + "-u", "type": "assistant", "message": map[string]any{"content": prefix + " answer", "stop_reason": "end_turn"}}}
	if child {
		rows = append(rows, map[string]any{"uuid": "child", "parentUuid": "", "type": "assistant", "isSidechain": true, "agentId": "child-agent", "message": map[string]any{"content": "excluded child payload", "stop_reason": "end_turn"}})
	}
	var b strings.Builder
	for _, row := range rows {
		row["sessionId"], row["version"], row["promptId"] = native, "2.1.0", prefix+"-prompt"
		if row["isSidechain"] == nil {
			row["isSidechain"] = false
		}
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	dir := filepath.Join(root, "projects", "workspace")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, native+".jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return int64(b.Len())
}

type captureProbe struct {
	limits []int64
	size   int64
	tail   string
}

func (p *captureProbe) ReadNativeTranscript(context.Context, ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	return ports.NativeTranscriptSegment{}, errors.New("unexpected pinned read during capture")
}
func (p *captureProbe) CaptureNativeTranscript(_ context.Context, r ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	p.limits = append(p.limits, r.MaxSourceBytes)
	if r.ByteCutoff != 0 || r.LeafID != "" || r.Schema != ports.ClaudeTranscriptSchemaV1 || len(r.KnownNativeIDs) != 2 {
		return ports.NativeTranscriptSegment{}, errors.New("capture request reused an unverified boundary")
	}
	return ports.NativeTranscriptSegment{NativeID: r.Source.NativeSessionID, Schema: r.Schema, ByteCutoff: p.size, LeafID: "leaf", TailState: p.tail}, nil
}

type readOnlyCaptureProbe struct{}

func (readOnlyCaptureProbe) ReadNativeTranscript(context.Context, ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	return ports.NativeTranscriptSegment{}, errors.New("capture must require a capability")
}

func TestTranscriptCaptureRequiresCapabilityAndBoundsCombinedBytes(t *testing.T) {
	for _, which := range []string{"capability", "combined size", "unsettled boundary", "unsupported history"} {
		t.Run(which, func(t *testing.T) {
			root := t.TempDir()
			snapshot := domain.TranscriptSourceSnapshot{Session: domain.SessionRecord{ID: "source", ProjectID: "project", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode}, Fingerprint: "owned", NativeSessions: []domain.TranscriptNativeSession{{Harness: domain.HarnessClaudeCode, NativeID: "11111111-1111-4111-8111-111111111111", ConfigDir: root}, {Harness: domain.HarnessClaudeCode, NativeID: "22222222-2222-4222-8222-222222222222", ConfigDir: root}}}
			probe := &captureProbe{size: ports.MaxTranscriptSourceBytes/2 + 1, tail: "complete"}
			var reader ports.NativeTranscriptReader = probe
			want := "TRANSCRIPT_LIMIT_EXCEEDED"
			switch which {
			case "capability":
				reader = readOnlyCaptureProbe{}
				want = "TRANSCRIPT_CAPTURE_UNAVAILABLE"
			case "unsettled boundary":
				probe.tail = "pending"
				want = "TRANSCRIPT_CUTOFF_INVALID"
			case "unsupported history":
				snapshot.NativeSessions[0].Harness = domain.HarnessCodex
				want = "ORCHESTRATOR_HANDOFF_HISTORY_UNSUPPORTED"
			}
			_, err := CaptureBoundaries(context.Background(), snapshot, reader)
			var native *ports.NativeTranscriptError
			if !errors.As(err, &native) || native.Code != want {
				t.Fatalf("capture=%v want %s", err, want)
			}
			if which == "combined size" && (len(probe.limits) != 2 || probe.limits[0] != ports.MaxTranscriptSourceBytes || probe.limits[1] != ports.MaxTranscriptSourceBytes-probe.size) {
				t.Fatalf("combined capacity was not propagated: %v", probe.limits)
			}
			if which == "unsupported history" && len(probe.limits) != 0 {
				t.Fatal("unsupported history reached native capture")
			}
		})
	}
}

func TestTranscriptCaptureOrdersEpochsByVerifiedAncestryThenBuildsFixedCutoffs(t *testing.T) {
	root := t.TempDir()
	old := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	next := "11111111-1111-4111-8111-111111111111"
	oldBytes := writeCaptureEpoch(t, root, old, "old", "", false)
	nextBytes := writeCaptureEpoch(t, root, next, "next", "old-a", true)
	snapshot := domain.TranscriptSourceSnapshot{Session: domain.SessionRecord{ID: "source", ProjectID: "project", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{AgentSessionID: next}}, Fingerprint: "durable-ao-snapshot", NativeSessions: []domain.TranscriptNativeSession{{Harness: domain.HarnessClaudeCode, NativeID: next, ConfigDir: root}, {Harness: domain.HarnessClaudeCode, NativeID: old, ConfigDir: root}}}
	cuts, err := CaptureBoundaries(context.Background(), snapshot, claudecode.New())
	if err != nil {
		t.Fatal(err)
	}
	if len(cuts) != 2 || cuts[0].NativeID != old || cuts[0].LeafID != "old-a" || cuts[0].Bytes != oldBytes || cuts[1].NativeID != next || cuts[1].LeafID != "next-a" || cuts[1].Bytes != nextBytes {
		t.Fatalf("capture mixed child/main or used lexical order: %+v", cuts)
	}
	out, err := Build(context.Background(), snapshot, cuts, claudecode.New())
	if err != nil {
		t.Fatal(err)
	}
	if out.Manifest.Coverage != "complete_for_scope" || !strings.Contains(string(out.Transcript), "old prompt") || !strings.Contains(string(out.Transcript), "next answer") || strings.Contains(string(out.Transcript), "excluded child payload") {
		t.Fatal("capture/build lost owned epoch or mixed child text")
	}
	file, err := os.OpenFile(filepath.Join(root, "projects", "workspace", next+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{}\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), snapshot, cuts, claudecode.New()); err == nil {
		t.Fatal("Build silently recaptured an advanced native boundary")
	}
}

func TestTranscriptCapturePinsNativeContentAcrossLaterBuild(t *testing.T) {
	root := t.TempDir()
	native := "11111111-1111-4111-8111-111111111111"
	writeCaptureEpoch(t, root, native, "old", "", false)
	snapshot := domain.TranscriptSourceSnapshot{Session: domain.SessionRecord{ID: "source", ProjectID: "project", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{ProviderConversationID: native}}, Fingerprint: "durable-source", NativeSessions: []domain.TranscriptNativeSession{{Harness: domain.HarnessClaudeCode, NativeID: native, ConfigDir: root}}}
	cuts, err := CaptureBoundaries(context.Background(), snapshot, claudecode.New())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "projects", "workspace", native+".jsonl")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(original), "old prompt", "new prompt", 1)
	if len(changed) != len(original) {
		t.Fatal("fixture changed size")
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Build(context.Background(), snapshot, cuts, claudecode.New())
	var nativeError *ports.NativeTranscriptError
	if !errors.As(err, &nativeError) || nativeError.Code != "TRANSCRIPT_SOURCE_CHANGED" {
		t.Fatalf("changed captured native prefix accepted: %v", err)
	}
}
