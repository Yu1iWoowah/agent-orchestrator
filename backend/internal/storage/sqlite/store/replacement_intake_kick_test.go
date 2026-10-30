package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementIntakeKickReceiptFencesCurrentWriter(t *testing.T) {
	for _, fault := range []string{"current", "unreleased", "revision", "generation", "capability", "lease epoch", "lease frozen"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeTransferFixture(t)
			ctx := context.Background()
			op := f.op
			if fault != "unreleased" {
				var err error
				var applied bool
				op, applied, err = f.api.ReleaseReplacementIntake(ctx, op.ID, op.Revision, time.Now())
				if err != nil || !applied {
					t.Fatal(applied, err)
				}
			}
			revision := op.Revision
			switch fault {
			case "revision":
				revision--
			case "generation", "capability":
				writer := f.writer
				if fault == "generation" {
					writer.Metadata.ControllerGeneration = "rotated"
				} else {
					writer.Metadata.BrowserCapabilityVerifier = "rotated"
				}
				if err := f.st.UpdateSession(ctx, writer); err != nil {
					t.Fatal(err)
				}
			case "lease epoch", "lease frozen":
				state, err := f.st.GetProjectDocumentState(ctx, op.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "lease epoch" {
					state.Lease.Epoch++
				} else {
					state.Lease.Frozen = true
				}
				if saved, err := f.st.SaveProjectDocumentState(ctx, op.ProjectID, state); err != nil || !saved {
					t.Fatal(saved, err)
				}
			}
			result, applied, err := f.st.RecordReplacementIntakeDrain(ctx, op.ID, revision, time.Now())
			if err != nil || applied != (fault == "current") {
				t.Fatal("kick receipt ignored current writer", fault, applied, err)
			}
			if fault == "current" {
				replay, applied, err := f.st.RecordReplacementIntakeDrain(ctx, op.ID, revision, time.Now())
				if err != nil || !applied || replay.Revision != result.Revision || !replay.IntakeDrainRequestedAt.Equal(*result.IntakeDrainRequestedAt) {
					t.Fatal("kick receipt replay changed identity", applied, err)
				}
			} else {
				current, _, err := f.st.GetOrchestratorReplacement(ctx, op.ID)
				if err != nil || current.IntakeDrainRequestedAt != nil || current.Revision != op.Revision {
					t.Fatal("refused kick changed operation", current.Phase, err)
				}
			}
		})
	}
}

func TestReplacementKickReceiptCannotBeForgedOrRewritten(t *testing.T) {
	f := newIntakeTransferFixture(t)
	ctx := context.Background()
	op, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now())
	if err != nil || !applied {
		t.Fatal(applied, err)
	}
	forged := op
	now := time.Now().UTC()
	forged.IntakeDrainRequestedAt = &now
	if saved, err := f.st.SaveOrchestratorReplacement(ctx, forged); saved || err == nil {
		t.Fatal("generic save forged successful queue kick", saved, err)
	}
	op, applied, err = f.st.RecordReplacementIntakeDrain(ctx, op.ID, op.Revision, now)
	if err != nil || !applied {
		t.Fatal(applied, err)
	}
	for _, drop := range []bool{false, true} {
		altered := op
		later := now.Add(time.Minute)
		altered.IntakeDrainRequestedAt = &later
		if drop {
			altered.IntakeDrainRequestedAt = nil
		}
		if saved, err := f.st.SaveOrchestratorReplacement(ctx, altered); saved || err == nil {
			t.Fatal("generic save rewrote durable kick", drop, saved, err)
		}
	}
}

func TestReplacementKickReceiptENOSPCRollsBack(t *testing.T) {
	f := newIntakeTransferFixture(t)
	ctx := context.Background()
	op, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now())
	if err != nil || !applied {
		t.Fatal(applied, err)
	}
	if _, err := f.raw.Exec(`CREATE TRIGGER refuse_intake_kick BEFORE UPDATE ON orchestrator_replacements WHEN json_type(NEW.state_json,'$.intakeDrainRequestedAt')='text' BEGIN SELECT RAISE(ABORT,'kick ENOSPC'); END`); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := f.st.RecordReplacementIntakeDrain(ctx, op.ID, op.Revision, time.Now()); applied || err == nil {
		t.Fatal("failed kick committed", applied, err)
	}
	after, _, err := f.st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || !reflect.DeepEqual(op, after) {
		t.Fatal("partial kick receipt", after.Phase, err)
	}
}

func TestReplacementKickDoesNotClearWorkerNotificationFailure(t *testing.T) {
	f := newIntakeTransferFixture(t)
	ctx := context.Background()
	op := f.op
	op.Error = &domain.ReplacementFailure{Code: "ORCHESTRATOR_HANDOFF_WORKER_NOTIFICATION_PENDING", Message: "worker notification pending", Retryable: true}
	op.ResumePhase = domain.ReplacementNotifying
	op.Notifications = []domain.ReplacementWorkerNotification{{SessionID: f.worker.ID, ContactID: "handoff-notice:" + op.ID + ":" + string(f.worker.ID), ObservedOwner: f.worker.ControllerOwner(), State: "pending", CreatedAt: time.Now().UTC()}}
	if saved, err := f.st.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	op, _, err := f.st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	op, applied, err := f.api.ReleaseReplacementIntake(ctx, op.ID, op.Revision, time.Now())
	if err != nil || !applied {
		t.Fatal(applied, err)
	}
	after, applied, err := f.st.RecordReplacementIntakeDrain(ctx, op.ID, op.Revision, time.Now())
	if err != nil || !applied || after.Phase != domain.ReplacementNotifying || !reflect.DeepEqual(op.Error, after.Error) || !reflect.DeepEqual(op.Notifications, after.Notifications) {
		t.Fatal("queue kick consumed worker notification", after.Phase, after.Error, err)
	}
}
