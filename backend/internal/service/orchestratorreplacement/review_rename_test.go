package orchestratorreplacement_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementProjectRenameAfterIntentPreservesExport(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.SourceStopIntentAt != nil })
	ctx := context.Background()
	p, _, err := x.f.store.GetProject(ctx, string(x.operation.ProjectID))
	if err != nil {
		t.Fatal(err)
	}
	if saved, err := x.f.store.UpdateProjectSettings(ctx, p.ID, "renamed during replacement", p.Config); err != nil || !saved {
		t.Fatal(saved, err)
	}
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementTranscriptVerified })
	if x.operation.SourceStop == nil || x.operation.Transcript.Receipt == nil || *x.actualStops != 1 {
		t.Fatal("rename lost verified evidence", x.operation.Phase)
	}
}
