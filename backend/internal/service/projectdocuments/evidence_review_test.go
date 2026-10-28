package projectdocuments_test

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

type blockingEvidenceReader struct {
	evidenceReader
	reads            atomic.Int32
	entered, release chan struct{}
}

func TestHandoffEvidenceRejectsOwnerRotationDuringNativePreflight(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in := evidenceInput(t, f)
	snapshot, err := f.db.LoadTranscriptSourceSnapshot(ctx, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	reader := &blockingEvidenceReader{entered: make(chan struct{}), release: make(chan struct{})}
	in.Export, err = transcriptexport.Build(ctx, snapshot, []transcriptexport.EpochCutoff{{NativeID: "native-1", Bytes: 10, LeafID: "u", Schema: ports.ClaudeTranscriptSchemaV1}}, reader)
	if err != nil {
		t.Fatal(err)
	}
	published := make(chan error, 1)
	go func() { _, err := f.service.PublishEvidence(ctx, "demo", in); published <- err }()
	select {
	case <-reader.entered:
	case <-ctx.Done():
		t.Fatal("revalidation did not start")
	}
	owner := f.owner
	owner.Metadata.ControllerGeneration = "rotated-during-native-read"
	err = f.db.UpdateSession(ctx, owner)
	close(reader.release)
	publicationErr := <-published
	if err != nil {
		t.Fatalf("owner rotation blocked by native preflight: %v", err)
	}
	if publicationErr == nil {
		t.Fatal("stale owner published after native preflight")
	}
	dir := filepath.Join(f.root, "projects/demo/orchestrator/handoffs", in.OperationID, in.AttemptID)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("stale owner occupied final evidence directory", err)
	}
}

func (r *blockingEvidenceReader) ReadNativeTranscript(ctx context.Context, request ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	if r.reads.Add(1) == 2 {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return ports.NativeTranscriptSegment{}, ctx.Err()
		}
	}
	return r.evidenceReader.ReadNativeTranscript(ctx, request)
}

func TestHandoffEvidenceNativeRevalidationDoesNotHoldWriter(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	in := evidenceInput(t, f)
	snapshot, err := f.db.LoadTranscriptSourceSnapshot(ctx, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	reader := &blockingEvidenceReader{entered: make(chan struct{}), release: make(chan struct{})}
	in.Export, err = transcriptexport.Build(ctx, snapshot, []transcriptexport.EpochCutoff{{NativeID: "native-1", Bytes: 10, LeafID: "u", Schema: ports.ClaudeTranscriptSchemaV1}}, reader)
	if err != nil {
		t.Fatal(err)
	}
	published := make(chan error, 1)
	go func() { _, err := f.service.PublishEvidence(ctx, "demo", in); published <- err }()
	select {
	case <-reader.entered:
	case <-ctx.Done():
		t.Fatal("revalidation did not start")
	}
	written := make(chan error, 1)
	go func() {
		written <- f.db.UpsertProject(ctx, domain.ProjectRecord{ID: "unrelated", Path: "/unrelated", RegisteredAt: time.Now()})
	}()
	blocked := false
	select {
	case err := <-written:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		blocked = true
	}
	close(reader.release)
	if err := <-published; err != nil {
		t.Error(err)
	}
	if blocked {
		if err := <-written; err != nil {
			t.Error(err)
		}
		t.Fatal("native strict re-read blocked an unrelated SQLite writer")
	}
}
