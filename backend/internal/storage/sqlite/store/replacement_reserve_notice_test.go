package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementReserveRejectsPendingNotifications(t *testing.T) {
	for _, change := range []string{"pending only", "phase and pending"} {
		t.Run(change, func(t *testing.T) {
			f := newIntakeTransferFixture(t)
			ctx := context.Background()
			workers := make([]domain.SessionRecord, 64)
			for i := range workers {
				var err error
				workers[i], err = f.st.CreateSession(ctx, sampleRecord(string(f.op.ProjectID)))
				if err != nil {
					t.Fatal(err)
				}
			}
			// Pending work is valid under the ordinary budget; only recovery
			// credits are unavailable to it. Retain this existing notice too.
			control := f.op
			control.Notifications = []domain.ReplacementWorkerNotification{{SessionID: f.worker.ID, ContactID: "handoff-notice:" + control.ID + ":" + string(f.worker.ID), ObservedOwner: f.worker.ControllerOwner(), State: "pending", CreatedAt: time.Now().UTC()}}
			if saved, err := f.st.SaveOrchestratorReplacement(ctx, control); err != nil || !saved {
				t.Fatal("ordinary budget rejected valid pending work", saved, err)
			}
			for i := 0; i < 1100; i++ {
				op, _, err := f.st.GetOrchestratorReplacement(ctx, f.op.ID)
				if err != nil {
					t.Fatal(err)
				}
				op.Error = &domain.ReplacementFailure{Code: "QUOTA_FIXTURE", Message: fmt.Sprint(i)}
				saved, err := f.st.SaveOrchestratorReplacement(ctx, op)
				if errors.Is(err, domain.ErrReplacementRepairCapacity) {
					break
				}
				if err != nil || !saved || i == 1099 {
					t.Fatal("could not exhaust ordinary metadata budget", saved, err)
				}
			}
			before, _, err := f.st.GetOrchestratorReplacement(ctx, f.op.ID)
			if err != nil || before.Revision != 1024 {
				t.Fatal(before.Revision, err)
			}
			accepted := 0
			for _, worker := range workers {
				op, _, err := f.st.GetOrchestratorReplacement(ctx, f.op.ID)
				if err != nil {
					t.Fatal(err)
				}
				op.Notifications = append(op.Notifications, domain.ReplacementWorkerNotification{SessionID: worker.ID, ContactID: "handoff-notice:" + op.ID + ":" + string(worker.ID), ObservedOwner: worker.ControllerOwner(), State: "pending", CreatedAt: time.Now().UTC()})
				if change == "phase and pending" {
					op.Phase = domain.ReplacementRecoveryRequired
				}
				saved, err := f.st.SaveOrchestratorReplacement(ctx, op)
				if saved {
					accepted++
				} else if !errors.Is(err, domain.ErrReplacementRepairCapacity) {
					t.Fatal("unexpected notification refusal", err)
				}
				if change == "phase and pending" {
					break
				}
			}
			current, _, err := f.st.GetOrchestratorReplacement(ctx, f.op.ID)
			if err != nil || accepted != 0 || current.Revision != before.Revision || len(current.Notifications) != len(before.Notifications) {
				t.Errorf("pending notices spent recovery credits: accepted=%d revision=%d/%d notices=%d err=%v", accepted, current.Revision, before.Revision, len(current.Notifications), err)
			}
			released, applied, err := f.api.ReleaseReplacementIntake(ctx, current.ID, current.Revision, time.Now())
			if err != nil || !applied || released.IntakeTransfer == nil {
				t.Fatalf("pending notices starved real intake release: applied=%t err=%v", applied, err)
			}
			if len(released.Notifications) != 1 || released.Notifications[0] != before.Notifications[0] {
				t.Fatal("release lost an existing pending receipt")
			}
		})
	}
}
