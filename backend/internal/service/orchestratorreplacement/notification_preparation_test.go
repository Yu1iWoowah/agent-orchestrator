package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementTaskPreparationCancellationDoesNotBlockCompletion(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker.IsTerminated = true
	if err := x.f.store.UpdateSession(ctx, worker); err != nil {
		t.Fatal(err)
	}
	preparation, err := x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: x.operation.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, IsTaskPreparation: true, ProvisionState: domain.SessionProvisionProvisioning, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Notifications) != 0 {
		t.Errorf("hidden task preparation was captured as a notification target: %+v", op.Notifications)
	}
	if deleted, err := x.f.store.DeleteTaskPreparation(ctx, preparation.ID); err != nil || !deleted {
		t.Fatalf("cancel uncaptured preparation: deleted=%v err=%v", deleted, err)
	}
	for range 4 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if op.Phase != domain.ReplacementCompleted {
		t.Fatalf("cancelled task preparation blocked completion: phase=%s", op.Phase)
	}
}

func TestReplacementCapturedTaskPreparationCancellationRetainsTermination(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil || len(op.Notifications) != 1 {
		t.Fatal("missing captured target", op.Notifications, err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Model an old activation which captured a hidden preparation. This flag
	// is not part of its controller owner; the sealed target stays unchanged.
	if _, err := raw.Exec(`UPDATE sessions SET is_task_preparation=1 WHERE id=?`, x.worker.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if deleted, err := x.f.store.DeleteTaskPreparation(ctx, x.worker.ID); err != nil || deleted {
			t.Fatalf("captured preparation must retain termination evidence: deleted=%v err=%v", deleted, err)
		}
		retained, found, err := x.f.store.GetSession(ctx, x.worker.ID)
		if err != nil || !found || !retained.IsTerminated || !retained.IsTaskPreparation {
			t.Fatalf("missing hidden terminated preparation: found=%v terminated=%v hidden=%v err=%v", found, retained.IsTerminated, retained.IsTaskPreparation, err)
		}
	}
	for range 4 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if op.Phase != domain.ReplacementCompleted || op.NotificationCompletion == nil || op.NotificationCompletion.SkippedCount != 1 {
		t.Fatalf("retained preparation did not settle via skipped: phase=%s completion=%+v", op.Phase, op.NotificationCompletion)
	}
	receipts, err := x.f.store.ReplacementWorkerNoticeReceipts(ctx, op.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != domain.ReplacementNoticeSkipped || receipts[0].EvidenceKind != "terminated" {
		t.Fatal("missing positive skipped receipt", receipts, err)
	}
}

func TestReplacementCorruptActiveOperationRetainsRollbackSeed(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	seed, err := x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: x.operation.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, ProvisionState: domain.SessionProvisionProvisioning, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE orchestrator_replacements SET state_json=json_set(state_json,'$.unknownPositiveFact',1) WHERE id=?`, x.operation.ID); err != nil {
		t.Fatal(err)
	}
	if deleted, err := x.f.store.DeleteSession(ctx, seed.ID); err != nil || deleted {
		t.Fatalf("unverified active operation must retain seed for rollback fallback: deleted=%v err=%v", deleted, err)
	}
	retained, found, err := x.f.store.GetSession(ctx, seed.ID)
	if err != nil || !found {
		t.Fatal("seed disappeared", found, err)
	}
	// Rollback's normal fallback remains able to record positive termination.
	retained.IsTerminated = true
	if err := x.f.store.UpdateSession(ctx, retained); err != nil {
		t.Fatal(err)
	}
	retained, found, err = x.f.store.GetSession(ctx, seed.ID)
	if err != nil || !found || !retained.IsTerminated {
		t.Fatal("rollback termination was not preserved", retained.IsTerminated, found, err)
	}
	if _, _, err := x.f.store.GetOrchestratorReplacement(ctx, x.operation.ID); err == nil {
		t.Fatal("seed cleanup laundered the corrupt operation")
	}
	if fenced, err := x.f.store.ReplacementWakeFenced(ctx, x.operation.SourceSessionID); err != nil || !fenced {
		t.Fatal("seed cleanup released the corrupt operation's fence", fenced, err)
	}
}
