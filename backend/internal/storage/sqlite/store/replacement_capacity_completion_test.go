package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementCapacityPreservesTransferDrainAndCompletion(t *testing.T) {
	// 3C facts come from the existing privileged fixture. This tests storage
	// capacity after those facts exist; it does not implement a 3C producer.
	f := newIntakeTransferFixture(t)
	ctx := context.Background()
	var op domain.OrchestratorReplacement
	for i := 0; i < 1200; i++ {
		var err error
		op, _, err = f.st.GetOrchestratorReplacement(ctx, f.op.ID)
		if err != nil {
			t.Fatal(err)
		}
		op.Error = &domain.ReplacementFailure{Code: "QUOTA_FIXTURE", Message: fmt.Sprint(i)}
		applied, err := f.st.SaveOrchestratorReplacement(ctx, op)
		if errors.Is(err, domain.ErrReplacementRepairCapacity) {
			break
		}
		if err != nil || !applied {
			t.Fatal(applied, err)
		}
		if i == 1199 {
			t.Fatal("metadata escaped the ordinary limit")
		}
	}
	op, _, err := f.st.GetOrchestratorReplacement(ctx, f.op.ID)
	if err != nil || op.Revision != 1024 {
		t.Fatal("ordinary metadata consumed recovery credits", op.Revision, err)
	}
	op, applied, err := f.api.ReleaseReplacementIntake(ctx, op.ID, op.Revision, time.Now())
	if err != nil || !applied || op.IntakeTransfer == nil {
		t.Fatalf("capacity blocked canonical intake transfer/release: applied=%t err=%v", applied, err)
	}
	op, applied, err = f.st.RecordReplacementIntakeDrain(ctx, op.ID, op.Revision, time.Now())
	if err != nil || !applied {
		t.Fatal("capacity blocked drain receipt", applied, err)
	}
	op.Phase, op.Error = domain.ReplacementCompleted, nil
	if applied, err := f.st.SaveOrchestratorReplacement(ctx, op); err != nil || !applied {
		t.Fatal("capacity blocked proven completion", applied, err)
	}
	current, found, err := f.st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || !found || current.Phase != domain.ReplacementCompleted || current.SourceStop == nil || current.WriterActivation == nil {
		t.Fatal("completion lost lifecycle facts", found, err)
	}
	if _, found, err := f.st.GetActiveOrchestratorReplacement(ctx, op.ProjectID); err != nil || found {
		t.Fatal("completion retained active reservation", found, err)
	}
}
