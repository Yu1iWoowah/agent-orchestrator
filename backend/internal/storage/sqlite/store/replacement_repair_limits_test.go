package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementOversizedCheckpointRollsBackBeforePublication(t *testing.T) {
	st, api, op, _ := repairFixture(t)
	ctx := context.Background()
	before, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = domain.ReplacementPreflight
	op.Error = &domain.ReplacementFailure{Code: "LARGE", Message: strings.Repeat("x", 16<<20)}
	if applied, err := st.SaveOrchestratorReplacement(ctx, op); applied || err == nil {
		t.Errorf("writer published a capsule larger than the reader accepts: applied=%t err=%v", applied, err)
	}
	after, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
	if err != nil || after.Corrupt || after.RawDigest != before.RawDigest || after.SnapshotRevision != before.SnapshotRevision {
		t.Fatalf("oversized capsule changed trusted state: corrupt=%t revision=%d err=%v", after.Corrupt, after.Revision, err)
	}
}

func TestReplacementCheckpointHistoryHasAnOperationLimit(t *testing.T) {
	st, api, op, raw := repairFixture(t)
	ctx := context.Background()
	// The retained history is immutable; a runaway writer must be bounded.
	const capsuleLimit = 1024
	op.Phase = domain.ReplacementPreflight
	for i := 1; i < capsuleLimit; i++ {
		op.UpdatedAt = op.CreatedAt.Add(time.Duration(i) * time.Millisecond)
		op.Error = &domain.ReplacementFailure{Code: "STEP", Message: fmt.Sprint(i)}
		if saved, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
			t.Fatalf("legitimate capsule %d refused: saved=%t err=%v", i+1, saved, err)
		}
		var err error
		op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	before, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	op.Error.Message = "beyond history budget"
	if saved, err := st.SaveOrchestratorReplacement(ctx, op); saved || err == nil {
		t.Errorf("snapshot history grew without a limit: saved=%t err=%v", saved, err)
	}
	after, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
	if err != nil || after.RawDigest != before.RawDigest || after.Corrupt {
		t.Fatal("capacity refusal did not roll back the operation", err)
	}
	var count int
	if err := raw.QueryRow(`SELECT count(*) FROM orchestrator_replacement_repair_snapshots WHERE operation_id=?`, op.ID).Scan(&count); err != nil || count != capsuleLimit {
		t.Fatal("capacity refusal changed immutable history", count, err)
	}
}
