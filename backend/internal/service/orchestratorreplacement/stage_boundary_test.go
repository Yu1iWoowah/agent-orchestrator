package orchestratorreplacement_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestPreparationStageStopsAtVerifiedEvidence(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementTranscriptVerified })
	before := x.operation
	for range 3 {
		op, err := x.service().Advance(context.Background(), before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if op.Phase != domain.ReplacementTranscriptVerified || op.Phase.Terminal() || op.Revision != before.Revision || op.SourceRetiredAt != nil || op.SuccessorSessionID != "" || op.Bootstrap != nil || op.ResumeEvidence != nil || op.WriterActivation != nil {
			t.Fatalf("preparation advanced beyond verified evidence: %+v", op)
		}
	}
}
