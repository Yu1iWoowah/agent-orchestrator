package orchestratorreplacement_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementVerifiedEvidenceHasSeparateRetirementIntent(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementTranscriptVerified })
	before := x.operation
	op, err := x.service().Advance(context.Background(), before.ID)
	if err != nil || op.Phase != domain.ReplacementRetiring || op.SourceRetiredAt != nil || op.SuccessorSessionID != "" || x.f.runtime.retirements != 0 {
		t.Fatalf("retirement lacked separate durable intent: phase=%s err=%v", op.Phase, err)
	}
}
