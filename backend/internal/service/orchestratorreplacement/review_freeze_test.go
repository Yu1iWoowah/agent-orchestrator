package orchestratorreplacement_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

func TestReplacementGenericSaveCannotMintReceiptAtExportBoundary(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool {
		return op.Transcript != nil && op.Transcript.CapturedAt != nil
	})
	op := x.operation
	op.Phase = domain.ReplacementTranscriptVerified
	op.Transcript.Receipt = &domain.HandoffEvidenceReceipt{Directory: t.TempDir(), ManifestHash: op.RequestHash, InputHash: op.RequestHash}
	op.Transcript.TranscriptHash = op.RequestHash
	if saved, err := x.f.store.SaveOrchestratorReplacement(context.Background(), op); saved || err == nil {
		t.Fatalf("generic save minted first receipt from a real frozen/captured operation: saved=%t err=%v", saved, err)
	}
}

func TestReplacementExportUsesFrozenHandoffBytesAfterLivingFileMutation(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.FrozenAt != nil })
	path := filepath.Join(x.operation.SharedDir, x.operation.DocumentPath)
	frozen, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Transcript.CapturedAt != nil })
	if err := os.WriteFile(path, []byte("raw mutation after verified freeze"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snapshot, err := x.f.store.Store.LoadTranscriptSourceSnapshot(ctx, x.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := transcriptexport.Build(ctx, snapshot, x.operation.Transcript.NativeCutoffs, x.reader)
	if err != nil {
		t.Fatal(err)
	}
	x.operation, err = x.documents.PublishReplacementEvidence(ctx, x.operation.ID, x.operation.Revision, out)
	if err != nil || x.operation.Phase != domain.ReplacementTranscriptVerified {
		t.Fatalf("verified frozen bytes depended on mutable living file: %v", err)
	}
	exported, err := os.ReadFile(filepath.Join(x.operation.Transcript.Receipt.Directory, "handoff.md"))
	if err != nil || string(exported) != string(frozen) {
		t.Fatal("export lost the verified frozen handoff snapshot", err)
	}
}
