package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementTUIHookAcceptanceSurvivesNextCheckpoint(t *testing.T) {
	for _, variant := range []string{"next prompt", "owner rotation", "stale signal"} {
		t.Run(variant, func(t *testing.T) {
			x, worker, op, claim := tuiNoticeFixture(t)
			ctx := context.Background()
			revision := worker.Revision
			worker.Metadata.ConversationCheckpointState = domain.ConversationCheckpointCoordination
			worker.Metadata.ConversationCheckpointGeneration = worker.Metadata.RuntimeLaunchID
			worker.Metadata.ConversationCheckpointNativeID = worker.Metadata.AgentSessionID
			worker.Metadata.ConversationCheckpointTurnID = claim.NativeDeliveryID()
			worker.UpdatedAt = time.Now()
			if variant == "stale signal" {
				revision--
			}
			changed, err := x.f.store.UpdateSessionFromActivitySignal(ctx, worker, revision)
			if err != nil || changed != (variant != "stale signal") {
				t.Fatal("hook CAS", changed, err)
			}
			worker, _, err = x.f.store.GetSession(ctx, worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			worker.Metadata.ConversationCheckpointState = domain.ConversationCheckpointPrompt
			worker.Metadata.ConversationCheckpointTurnID = "next-human-prompt"
			if variant == "owner rotation" {
				worker.Metadata.RuntimeLaunchID = "next-launch"
				worker.Metadata.AgentSessionIDLaunchID = "next-launch"
			}
			if err := x.f.store.UpdateSession(ctx, worker); err != nil {
				t.Fatal(err)
			}
			receipts, err := x.f.store.ReplacementWorkerNoticeReceipts(ctx, op.ID)
			if err != nil || len(receipts) != 1 {
				t.Fatal(receipts, err)
			}
			if variant == "stale signal" {
				if receipts[0].State != domain.ReplacementNoticeUncertain {
					t.Fatal("stale hook minted acceptance", receipts)
				}
				return
			}
			if receipts[0].State != domain.ReplacementNoticeAccepted || receipts[0].TurnID != claim.NativeDeliveryID() {
				t.Fatal("positive hook lost before reconcile", receipts)
			}
			op, err = x.service().Advance(ctx, op.ID)
			if err != nil || op.Phase != domain.ReplacementCompleted {
				t.Fatal("accepted hook did not complete", op.Phase, err)
			}
		})
	}
}

func TestReplacementNoWriteOutcomeSurvivesRecovery(t *testing.T) {
	x, worker, op, claim := tuiNoticeFixture(t)
	ctx := context.Background()
	op.Phase = domain.ReplacementRecoveryRequired
	op.ResumePhase = domain.ReplacementNotifying
	op.Error = &domain.ReplacementFailure{Code: "TEST_RECOVERY", Message: "fixture"}
	op.UpdatedAt = time.Now()
	if changed, err := x.f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !changed {
		t.Fatal(changed, err)
	}
	api := any(x.f.store).(noticeAttemptStore)
	if changed, err := api.FinishReplacementWorkerNotice(ctx, op.ID, worker.ID, claim.AttemptID, claim.Revision, false, time.Now()); err != nil || !changed {
		t.Fatal("positive no-write lost after recovery", changed, err)
	}
	receipts, err := x.f.store.ReplacementWorkerNoticeReceipts(ctx, op.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != domain.ReplacementNoticePending || receipts[0].OutcomeRecordedAt == nil {
		t.Fatal(receipts, err)
	}
	if _, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision+1, worker.ID, worker.ControllerOwner(), time.Now()); err != nil || send {
		t.Fatal("outcome granted new send authority", send, err)
	}
}

func TestReplacementNoticeRetryBudgetIsPerTarget(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	other := worker
	other.ID = ""
	other, err = x.f.store.CreateSession(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range []domain.SessionRecord{worker, other} {
		if _, err := x.f.store.CreateConversation(ctx, []string{"quota-a", "quota-b"}[i], domain.ConversationScopeSession, x.operation.ProjectID, w.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	op := x.operation
	for range 3 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	api := any(x.f.store).(noticeAttemptStore)
	exhausted := false
	for range 80 {
		claim, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, worker.ID, worker.ControllerOwner(), time.Now())
		if errors.Is(err, domain.ErrReplacementNoticeCapacity) {
			exhausted = true
			break
		}
		if err != nil || !send {
			t.Fatal(send, err)
		}
		if changed, err := api.FinishReplacementWorkerNotice(ctx, op.ID, worker.ID, claim.AttemptID, claim.Revision, false, time.Now()); err != nil || !changed {
			t.Fatal(changed, err)
		}
	}
	if !exhausted {
		t.Fatal("retry budget unbounded")
	}
	if _, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, other.ID, other.ControllerOwner(), time.Now()); err != nil || !send {
		t.Fatal("one worker exhausted another target's budget", send, err)
	}
	receipts, err := x.f.store.ReplacementWorkerNoticeReceipts(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range receipts {
		if r.SessionID == worker.ID && r.ErrorCode != "DELIVERY_RETRY_CAPACITY" {
			t.Fatal("exhausted target not identified", r)
		}
		if r.SessionID == other.ID && r.ErrorCode == "DELIVERY_RETRY_CAPACITY" {
			t.Fatal("unrelated target reports exhaustion", r)
		}
	}
}

func TestReplacementTUIHookAcceptanceRollsBackWithHistory(t *testing.T) {
	x, worker, op, claim := tuiNoticeFixture(t)
	ctx := context.Background()
	raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER audit_notice_history_enospc BEFORE INSERT ON orchestrator_replacement_notice_history BEGIN SELECT RAISE(ABORT,'ENOSPC injected'); END`); err != nil {
		t.Fatal(err)
	}
	originalRevision := worker.Revision
	worker.Metadata.ConversationCheckpointState = domain.ConversationCheckpointCoordination
	worker.Metadata.ConversationCheckpointGeneration = worker.Metadata.RuntimeLaunchID
	worker.Metadata.ConversationCheckpointNativeID = worker.Metadata.AgentSessionID
	worker.Metadata.ConversationCheckpointTurnID = claim.NativeDeliveryID()
	worker.UpdatedAt = time.Now()
	if changed, err := x.f.store.UpdateSessionFromActivitySignal(ctx, worker, originalRevision); err == nil || changed {
		t.Fatal("hook committed without durable receipt", changed, err)
	}
	actual, _, err := x.f.store.GetSession(ctx, worker.ID)
	if err != nil || actual.Revision != originalRevision || actual.Metadata.ConversationCheckpointTurnID == claim.NativeDeliveryID() {
		t.Fatal("activity escaped transaction rollback", actual, err)
	}
	receipts, err := x.f.store.ReplacementWorkerNoticeReceipts(ctx, op.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != domain.ReplacementNoticeUncertain || receipts[0].Revision != claim.Revision {
		t.Fatal(receipts, err)
	}
	if _, err := raw.Exec(`DROP TRIGGER audit_notice_history_enospc`); err != nil {
		t.Fatal(err)
	}
	if changed, err := x.f.store.UpdateSessionFromActivitySignal(ctx, worker, originalRevision); err != nil || !changed {
		t.Fatal("same hook could not recover", changed, err)
	}
	receipts, err = x.f.store.ReplacementWorkerNoticeReceipts(ctx, op.ID)
	if err != nil || receipts[0].State != domain.ReplacementNoticeAccepted {
		t.Fatal(receipts, err)
	}
}
