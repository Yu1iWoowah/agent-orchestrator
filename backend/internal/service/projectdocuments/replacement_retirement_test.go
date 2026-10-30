package projectdocuments_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

func TestReplacementRetirementProofBindsPublishedFilesAndBoundedWitness(t *testing.T) {
	for _, changed := range []string{"current", "handoff.md", "transcript.md", "manifest.json", "living handoff", "living after proof", "prefix after proof", "root replacement after proof", "cancelled check"} {
		t.Run(changed, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			op, out := replacementEvidence(t, f, true)
			op, err := f.service.PublishReplacementEvidence(ctx, op.ID, op.Revision, out)
			if err != nil {
				t.Fatal(err)
			}
			op.Phase = domain.ReplacementRetiring
			if ok, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
				t.Fatal(ok, err)
			}
			op, _, err = f.db.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(op.Transcript.Receipt.Directory, changed)
			if changed == "living handoff" {
				path = filepath.Join(op.SharedDir, op.DocumentPath)
			}
			if changed == "handoff.md" || changed == "transcript.md" || changed == "manifest.json" || changed == "living handoff" {
				if err := os.WriteFile(path, []byte("tampered evidence"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			proof, err := f.service.PrepareReplacementRetirement(ctx, op.ID, op.Revision, out)
			if changed == "handoff.md" || changed == "transcript.md" || changed == "manifest.json" || changed == "living handoff" {
				if err == nil {
					t.Fatalf("%s produced retirement proof", changed)
				}
				return
			}
			if err != nil || proof.OperationID() != op.ID || proof.OperationRevision() != op.Revision || proof.Source().ControllerOwner() != op.SourceOwner || proof.DocumentRevision() <= 0 {
				t.Fatalf("retirement evidence not bound: err=%v", err)
			}
			if changed == "prefix after proof" || changed == "living after proof" {
				path = filepath.Join(op.Transcript.Receipt.Directory, "transcript.md")
				if changed == "living after proof" {
					path = filepath.Join(op.SharedDir, op.DocumentPath)
				}
				before, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteAt([]byte("X"), 0)
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatal(writeErr, closeErr)
				}
				if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			if changed == "root replacement after proof" {
				old := op.Transcript.Receipt.Directory + ".old"
				if err := os.Rename(op.Transcript.Receipt.Directory, old); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(op.Transcript.Receipt.Directory, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"manifest.json", "handoff.md", "transcript.md"} {
					data, err := os.ReadFile(filepath.Join(old, name))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(op.Transcript.Receipt.Directory, name), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			checkContext := ctx
			if changed == "cancelled check" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				checkContext = cancelled
			}
			err = proof.Check(checkContext)
			if (err == nil) != (changed == "current") {
				t.Fatalf("%s witness result=%v", changed, err)
			}
			if changed == "current" {
				result, applied, err := f.db.CommitReplacementRetirement(ctx, op.ID, op.Revision, proof.Source(), proof.DocumentRevision(), proof.Check)
				if err != nil || !applied || result.SourceRetiredAt == nil || result.Phase != domain.ReplacementStarting {
					t.Fatalf("verified proof could not commit: applied=%t phase=%s err=%v", applied, result.Phase, err)
				}
			} else {
				if _, applied, err := f.db.CommitReplacementRetirement(checkContext, op.ID, op.Revision, proof.Source(), proof.DocumentRevision(), proof.Check); err == nil || applied {
					t.Fatalf("changed witness retired source: applied=%t err=%v", applied, err)
				}
				source, _, err := f.db.GetSession(ctx, op.SourceSessionID)
				if err != nil || source.IsTerminated {
					t.Fatal("failed retirement did not preserve source", err)
				}
			}
		})
	}
}

func TestReplacementRetirementProofZeroValueCannotAuthorizeCommit(t *testing.T) {
	var proof docs.ReplacementRetirementProof
	if err := proof.Check(context.Background()); err == nil {
		t.Fatal("zero proof accepted")
	}
}

type retirementCountingReader struct{ reads *int }

func (r retirementCountingReader) ReadNativeTranscript(ctx context.Context, request ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	*r.reads++
	return (evidenceReader{}).ReadNativeTranscript(ctx, request)
}

func TestReplacementRetirementReservationDoesNotParseNativeTranscript(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op, _ := replacementEvidence(t, f, true)
	snapshot, err := f.db.LoadTranscriptSourceSnapshot(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	out, err := transcriptexport.Build(ctx, snapshot, op.Transcript.NativeCutoffs, retirementCountingReader{reads: &reads})
	if err != nil {
		t.Fatal(err)
	}
	op, err = f.service.PublishReplacementEvidence(ctx, op.ID, op.Revision, out)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = domain.ReplacementRetiring
	if ok, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
		t.Fatal(ok, err)
	}
	op, _, err = f.db.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := reads
	proof, err := f.service.PrepareReplacementRetirement(ctx, op.ID, op.Revision, out)
	if err != nil || reads <= before {
		t.Fatalf("preflight did not re-read native evidence: reads=%d before=%d err=%v", reads, before, err)
	}
	before = reads
	if _, applied, err := f.db.CommitReplacementRetirement(ctx, op.ID, op.Revision, proof.Source(), proof.DocumentRevision(), proof.Check); err != nil || !applied {
		t.Fatal(applied, err)
	}
	if reads != before {
		t.Fatalf("native reader ran inside retirement reservation: before=%d after=%d", before, reads)
	}
}
