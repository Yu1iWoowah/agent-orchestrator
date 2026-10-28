package projectdocuments_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

type evidenceReader struct{}

func (evidenceReader) ReadNativeTranscript(_ context.Context, r ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	return ports.NativeTranscriptSegment{CheckStability: func(ctx context.Context) error { return ctx.Err() }, NativeID: r.Source.NativeSessionID, ByteCutoff: r.ByteCutoff, LeafID: r.LeafID, SHA256: strings.Repeat("a", 64), Schema: r.Schema, ReaderVersion: "fixture/1", MainRecordIDs: []string{"u"}, Records: []ports.NativeTranscriptRecord{{ID: "u", Type: "user", NativeID: r.Source.NativeSessionID, Provenance: domain.NativeThreadMain, Blocks: []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: "preserved source"}}}}}, nil
}
func evidenceInput(t *testing.T, f fixture) docs.EvidenceInput {
	t.Helper()
	ctx := context.Background()
	g, e := f.service.Acquire(ctx, "demo", f.actor)
	if e != nil {
		t.Fatal(e)
	}
	doc, e := f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: g, Path: "handoff/orchestrator-handoff.md", Content: "# frozen handoff", ExpectedHash: docs.MissingHash})
	if e != nil {
		t.Fatal(e)
	}
	if e := f.service.Freeze(ctx, "demo", f.actor, g); e != nil {
		t.Fatal(e)
	}
	// Retained native metadata is supplied by an actual owned usage source below.
	if _, e = f.db.UpsertUsageBinding(ctx, domain.UsageBindingRecord{SessionID: f.owner.ID, Harness: domain.HarnessClaudeCode, NativeRootID: "native-1"}); e != nil {
		t.Fatal(e)
	}
	s, e := f.db.LoadTranscriptSourceSnapshot(ctx, f.owner.ID)
	if e != nil {
		t.Fatal(e)
	}
	// A missing config-root inventory must fail closed; this fixture adds a durable
	// source using the same path layout the observer records.
	binding := s.Bindings[0]
	if _, e = f.db.InsertUsageSource(ctx, domain.UsageSourceRecord{BindingID: binding.ID, Kind: domain.UsageSourceClaudeMain, NativeSessionID: "native-1", ArtifactPath: "/fixture/projects/workspace/native-1.jsonl", Generation: 1}); e != nil {
		t.Fatal(e)
	}
	s, e = f.db.LoadTranscriptSourceSnapshot(ctx, f.owner.ID)
	if e != nil {
		t.Fatal(e)
	}
	out, e := transcriptexport.Build(ctx, s, []transcriptexport.EpochCutoff{{NativeID: "native-1", Bytes: 10, LeafID: "u", Schema: ports.ClaudeTranscriptSchemaV1}}, evidenceReader{})
	if e != nil {
		t.Fatal(e)
	}
	return docs.EvidenceInput{Actor: f.actor, Lease: g, OperationID: "11111111-1111-4111-8111-111111111111", AttemptID: "22222222-2222-4222-8222-222222222222", HandoffPath: doc.Path, HandoffHash: doc.Hash, Export: out, CreatedAt: time.Date(2026, 10, 28, 1, 2, 3, 0, time.UTC)}
}
func TestImmutableHandoffEvidence(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	in := evidenceInput(t, f)
	receipt, e := f.service.PublishEvidence(ctx, "demo", in)
	if e != nil {
		t.Fatal(e)
	}
	again, e := f.service.PublishEvidence(ctx, "demo", in)
	if e != nil || receipt != again {
		t.Fatalf("retry differs: %+v %v", again, e)
	}
	got, e := f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "transcript.md")
	if e != nil || !strings.Contains(got.Content, "preserved source") {
		t.Fatalf("read: %+v %v", got, e)
	}
	changed := in
	changed.CreatedAt = changed.CreatedAt.Add(time.Second)
	if _, e = f.service.PublishEvidence(ctx, "demo", changed); e == nil {
		t.Fatal("same attempt overwritten")
	}
	if e := os.WriteFile(filepath.Join(receipt.Directory, "transcript.md"), []byte("tampered"), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e = f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "transcript.md"); e == nil {
		t.Fatal("corrupt bytes accepted")
	}
	for _, p := range []string{"../manifest.json", "%2e%2e/transcript.md", "other.md"} {
		if _, e = f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, p); e == nil {
			t.Fatal("unsafe path accepted")
		}
	}
}

type largeEvidenceReader struct{ evidenceReader }

func (r largeEvidenceReader) ReadNativeTranscript(ctx context.Context, in ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	s, e := r.evidenceReader.ReadNativeTranscript(ctx, in)
	for i := 0; i < 12000; i++ {
		s.Records = append(s.Records, ports.NativeTranscriptRecord{Line: i + 2, ID: fmt.Sprint("child-", i), NativeID: in.Source.NativeSessionID, AgentID: strings.Repeat("c", 180), Type: "assistant", Provenance: domain.NativeThreadChild, ProvenanceRule: "explicit-sidechain", Excluded: "child"})
	}
	s.Counts.ChildRecords = 12000
	s.Counts.Records = 12001
	return s, e
}
func TestHandoffEvidenceRecoversLargeReceiptAfterCompletionENOSPC(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	in := evidenceInput(t, f)
	snapshot, e := f.db.LoadTranscriptSourceSnapshot(ctx, f.owner.ID)
	if e != nil {
		t.Fatal(e)
	}
	in.Export, e = transcriptexport.Build(ctx, snapshot, []transcriptexport.EpochCutoff{{NativeID: "native-1", Bytes: 10, LeafID: "u", Schema: ports.ClaudeTranscriptSchemaV1}}, largeEvidenceReader{})
	if e != nil {
		t.Fatal(e)
	}
	fault := &adoptionFaultStore{Store: f.db, failSaveAt: 1}
	svc := docs.New(docs.Deps{Store: fault, DataDir: f.root, Capabilities: capability{}})
	if _, e = svc.PublishEvidence(ctx, "demo", in); !errors.Is(e, syscall.ENOSPC) {
		t.Fatalf("expected completion ENOSPC: %v", e)
	}
	dir := filepath.Join(f.root, "projects/demo/orchestrator/handoffs", in.OperationID, in.AttemptID)
	before, e := os.Stat(filepath.Join(dir, "manifest.json"))
	if e != nil || before.Size() <= docs.MaxFileBytes {
		t.Fatalf("large manifest not published: %v %v", before, e)
	}
	if _, e = f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "transcript.md"); e == nil {
		t.Fatal("uncommitted receipt became visible")
	}
	receipt, e := f.service.PublishEvidence(ctx, "demo", in)
	if e != nil {
		t.Fatal(e)
	}
	after, e := os.Stat(filepath.Join(dir, "manifest.json"))
	if e != nil || before.ModTime() != after.ModTime() {
		t.Fatal("recovery rewrote immutable manifest")
	}
	if _, e = f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "manifest.json"); e == nil {
		t.Fatal("oversize preview accepted")
	}
	file, e := f.service.OpenEvidence(ctx, "demo", in.OperationID, in.AttemptID, "manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	_ = file.Close()
	// Evidence ownership is project/operation based and survives deleting source.
	if _, e = f.db.DeleteSession(ctx, f.owner.ID); e != nil {
		t.Fatal(e)
	}
	got, e := f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "transcript.md")
	if e != nil || got.Hash == "" || receipt.ManifestHash == "" {
		t.Fatal("source deletion lost evidence", e)
	}
}
func TestHandoffEvidenceRejectsChangedSourceAndUnfrozenLease(t *testing.T) {
	for _, change := range []string{"snapshot", "observation", "owner", "unfreeze", "cancel", "tamper-export"} {
		t.Run(change, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			in := evidenceInput(t, f)
			switch change {
			case "snapshot":
				_, e := f.db.UpsertUsageBinding(ctx, domain.UsageBindingRecord{SessionID: f.owner.ID, Harness: domain.HarnessClaudeCode, NativeRootID: "new-epoch"})
				if e != nil {
					t.Fatal(e)
				}
			case "observation":
				rec, _, e := f.db.GetSession(ctx, f.owner.ID)
				if e != nil {
					t.Fatal(e)
				}
				rec.Metadata.NativeCheckpointEvidence = domain.AppendNativeCheckpoint("", "native-1", domain.NativeCheckpointObservation{Generation: "new", PromptID: "not-flushed"})
				if e = f.db.UpdateSession(ctx, rec); e != nil {
					t.Fatal(e)
				}
			case "owner":
				rec, _, e := f.db.GetSession(ctx, f.owner.ID)
				if e != nil {
					t.Fatal(e)
				}
				rec.Metadata.ControllerGeneration = "new-owner"
				if e := f.db.UpdateSession(ctx, rec); e != nil {
					t.Fatal(e)
				}
			case "unfreeze":
				s, e := f.db.GetProjectDocumentState(ctx, "demo")
				if e != nil {
					t.Fatal(e)
				}
				s.Lease.Frozen = false
				if _, e = f.db.SaveProjectDocumentState(ctx, "demo", s); e != nil {
					t.Fatal(e)
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "tamper-export":
				in.Export.Transcript = append(in.Export.Transcript, []byte("changed")...)
			}
			if _, e := f.service.PublishEvidence(ctx, "demo", in); e == nil {
				t.Fatal("stale export published")
			}
			if _, e := f.service.ReadEvidence(context.Background(), "demo", in.OperationID, in.AttemptID, "transcript.md"); e == nil {
				t.Fatal("failed publication is readable")
			}
		})
	}
}

func TestHandoffEvidenceRejectsRotationDuringFinalAuthorization(t *testing.T) {
	f := setup(t)
	in := evidenceInput(t, f)
	hook := &finalAuthorizationRotationStore{Store: f.db, owner: f.owner, calls: 1}
	svc := docs.New(docs.Deps{Store: hook, DataDir: f.root, Capabilities: capability{}})
	if _, e := svc.PublishEvidence(context.Background(), "demo", in); e == nil {
		t.Fatal("stale controller published evidence during verifier rotation")
	}
	if _, e := os.Stat(filepath.Join(f.root, "projects/demo/orchestrator/handoffs", in.OperationID, in.AttemptID)); !os.IsNotExist(e) {
		t.Fatal("failed authorization occupied final evidence directory", e)
	}
}

func TestHandoffEvidenceRejectsProviderInputChangedWithoutTimelineEvent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := time.Now()
	conversation, err := f.db.CreateConversation(ctx, "batch", domain.ConversationScopeProject, "demo", f.owner.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.AppendUserMessage(ctx, conversation.ID, f.owner.ID, "epoch-1", domain.ConversationMessage{ID: "queued-message", ClientMessageID: "queued-client", Text: "queued", Origin: domain.MessageOriginHuman}, "queued-turn", now); err != nil {
		t.Fatal(err)
	}
	in := evidenceInput(t, f)
	if err = f.db.RecordProviderInput(ctx, "queued-turn", "new captured batch input"); err != nil {
		t.Fatal(err)
	}
	after, err := f.db.LoadTranscriptSourceSnapshot(ctx, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CutoffSequence != in.Export.Manifest.CutoffSequence {
		t.Fatal("fixture added a timeline event instead of changing only the input receipt")
	}
	_, err = f.service.PublishEvidence(ctx, "demo", in)
	var apiError *apierr.Error
	if !errors.As(err, &apiError) || apiError.Code != "HANDOFF_EVIDENCE_SOURCE_CHANGED" {
		t.Fatalf("changed batch input published: %v", err)
	}
	if _, err = f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "transcript.md"); err == nil {
		t.Fatal("stale evidence became readable")
	}
}

func TestEvidenceCannotBeEditedThroughSharedRoot(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	in := evidenceInput(t, f)
	receipt, e := f.service.PublishEvidence(ctx, "demo", in)
	if e != nil {
		t.Fatal(e)
	}
	state, e := f.db.GetProjectDocumentState(ctx, "demo")
	if e != nil {
		t.Fatal(e)
	}
	state.Lease.Frozen = false
	if _, e = f.db.SaveProjectDocumentState(ctx, "demo", state); e != nil {
		t.Fatal(e)
	}
	shared := filepath.Join(f.root, "projects/demo/orchestrator/shared")
	if e = os.Symlink(receipt.Directory, filepath.Join(shared, "evidence-link")); e != nil {
		t.Skip(e)
	}
	if _, e = f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: in.Lease, Path: "evidence-link/transcript.md", Content: "changed", ExpectedHash: docs.MissingHash}); e == nil {
		t.Fatal("generic shared write reached evidence")
	}
	got, e := f.service.ReadEvidence(ctx, "demo", in.OperationID, in.AttemptID, "transcript.md")
	if e != nil || strings.Contains(got.Content, "changed") {
		t.Fatal("immutable evidence changed", e)
	}
}
