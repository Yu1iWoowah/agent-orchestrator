package claudecode

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestClaudeTranscriptCaptureSelectsUniqueMainLeaf(t *testing.T) {
	child := mainRow("child", "", "assistant", "child content")
	child["isSidechain"], child["agentId"] = true, "subagent"
	stop := mainRow("stop", "a", "system", nil)
	stop["subtype"] = "stop_hook_summary"
	fixed := transcriptFixture(t, []map[string]any{mainRow("u", "", "user", "source prompt"), mainRow("a", "u", "assistant", "source answer"), child, stop, {"type": "queue-operation", "operation": "dequeue"}})
	capture := fixed
	capture.ByteCutoff, capture.LeafID = 0, ""
	segment, err := New().CaptureNativeTranscript(context.Background(), capture)
	if err != nil {
		t.Fatal(err)
	}
	if segment.LeafID != "a" || segment.ByteCutoff != fixed.ByteCutoff || segment.TailState != "complete" || !reflect.DeepEqual(segment.MainRecordIDs, []string{"u", "a"}) || segment.Counts.ChildRecords != 1 || segment.Counts.MetadataRecords != 2 || segment.CheckStability == nil {
		t.Fatalf("capture selected physical tail/child instead of main leaf: %+v", segment)
	}
	if err := segment.CheckStability(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixed.LeafID = segment.LeafID
	read, err := New().ReadNativeTranscript(context.Background(), fixed)
	if err != nil || read.SHA256 != segment.SHA256 || !reflect.DeepEqual(read.MainRecordIDs, segment.MainRecordIDs) {
		t.Fatal("capture differs from pinned reader", err)
	}
}

func TestClaudeTranscriptCaptureRejectsUnsettledAmbiguousOrUnknownTail(t *testing.T) {
	for _, which := range []string{"fork", "pending user", "pending hook", "unknown", "caller leaf"} {
		t.Run(which, func(t *testing.T) {
			rows := []map[string]any{mainRow("u", "", "user", "prompt"), mainRow("a", "u", "assistant", "answer")}
			want := "TRANSCRIPT_TAIL_UNSETTLED"
			switch which {
			case "fork":
				rows = append(rows, mainRow("b", "u", "assistant", "another branch"))
				want = "TRANSCRIPT_CUTOFF_AMBIGUOUS"
			case "pending user":
				rows = append(rows, mainRow("later", "a", "user", "not yet answered"))
			case "pending hook":
				stop := mainRow("stop", "a", "system", nil)
				stop["subtype"], stop["preventedContinuation"] = "stop_hook_summary", true
				rows = append(rows, stop)
			case "unknown":
				row := mainRow("future", "a", "system", nil)
				row["subtype"] = "unknown_future"
				rows = append(rows, row)
				want = "TRANSCRIPT_SCHEMA_UNSUPPORTED"
			case "caller leaf":
				want = "TRANSCRIPT_CUTOFF_INVALID"
			}
			request := transcriptFixture(t, rows)
			request.ByteCutoff, request.LeafID = 0, ""
			if which == "caller leaf" {
				request.LeafID = "a"
			}
			_, err := New().CaptureNativeTranscript(context.Background(), request)
			var native *ports.NativeTranscriptError
			if !errors.As(err, &native) || native.Code != want {
				t.Fatalf("%s capture=%v want %s", which, err, want)
			}
		})
	}
}

func TestClaudeTranscriptCaptureKeepsMetadataAncestryAndChildCollisionSeparate(t *testing.T) {
	rows := []map[string]any{mainRow("u", "", "user", "owned prompt")}
	parent := "u"
	for i := range 32 {
		id := fmt.Sprintf("metadata-%d", i)
		row := mainRow(id, parent, "progress", nil)
		row["data"] = map[string]any{"type": "hook_progress", "hookEvent": "PostToolUse"}
		rows = append(rows, row)
		parent = id
	}
	rows = append(rows, mainRow("a", parent, "assistant", "owned final"))
	child := mainRow("a", "", "assistant", "child reuses UUID")
	child["isSidechain"], child["agentId"] = true, "child-agent"
	rows = append(rows, child)
	request := transcriptFixture(t, rows)
	request.ByteCutoff, request.LeafID = 0, ""
	segment, err := New().CaptureNativeTranscript(context.Background(), request)
	if err != nil || segment.LeafID != "a" || !reflect.DeepEqual(segment.MainRecordIDs, []string{"u", "a"}) || segment.Counts.MetadataRecords != 32 || segment.Counts.ChildRecords != 1 {
		t.Fatalf("capture mixed provenance or broke metadata lineage: %+v %v", segment.Counts, err)
	}
}

func TestClaudeTranscriptCaptureRequiresVerifiedEpochParent(t *testing.T) {
	request := transcriptFixture(t, []map[string]any{mainRow("u", "parent", "user", "current prompt"), mainRow("a", "u", "assistant", "current answer")})
	request.ByteCutoff, request.LeafID = 0, ""
	request.KnownNativeIDs = []string{exportNativeID, "22222222-2222-4222-8222-222222222222"}
	_, err := New().CaptureNativeTranscript(context.Background(), request)
	var native *ports.NativeTranscriptError
	if !errors.As(err, &native) || native.Code != "TRANSCRIPT_ANCESTOR_REQUIRED" {
		t.Fatalf("missing parent not deferred: %v", err)
	}
	request.VerifiedAncestors = []ports.NativeTranscriptRecord{{ID: "parent", NativeID: request.KnownNativeIDs[1], RecordHash: "verified-parent-hash", Type: "assistant", State: "complete", Provenance: domain.NativeThreadMain}}
	segment, err := New().CaptureNativeTranscript(context.Background(), request)
	if err != nil || segment.LeafID != "a" || !reflect.DeepEqual(segment.MainRecordIDs, []string{"u", "a"}) {
		t.Fatalf("verified parent not reused: %+v %v", segment.MainRecordIDs, err)
	}
}
