package orchestratorreplacement_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

func TestReplacementDormantChatFYIQueuesWithoutWake(t *testing.T) {
	for _, state := range []string{"hibernated", "provisioning", "failed"} {
		t.Run(state, func(t *testing.T) {
			x := activationCoordinatorFixture(t)
			ctx := context.Background()
			if state == "hibernated" {
				if _, err := x.f.store.CreateConversation(ctx, "dormant-worker", domain.ConversationScopeSession, x.operation.ProjectID, x.worker.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
				worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				if changed, err := x.f.store.SetSessionHibernated(ctx, worker.ID, worker.Revision, &now); err != nil || !changed {
					t.Fatal("hibernate fixture", changed, err)
				}
			} else {
				provision := domain.SessionProvisionProvisioning
				if state == "failed" {
					provision = domain.SessionProvisionFailed
				}
				if changed, err := x.f.store.SetSessionProvisionState(ctx, x.worker.ID, provision, "fixture", time.Now()); err != nil || !changed {
					t.Fatal("provision fixture", changed, err)
				}
			}
			before, _, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil || len(op.Notifications) != 1 {
				t.Fatal("activation", op.Phase, op.Notifications, err)
			}
			for range 2 {
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			// No runtime, Chat service or agent is available to wake/dispatch.
			manager := sessionmanager.New(sessionmanager.Deps{Store: x.f.store.Store})
			for range 3 {
				if err := manager.DeliverReplacementWorkerFYI(ctx, op, before.ID); err != nil {
					t.Fatal("dormant queue admission", err)
				}
			}
			conversation, err := x.f.store.ConversationForSession(ctx, before.ID)
			if err != nil {
				t.Fatal("dormant worker has no durable FYI queue", err)
			}
			messages, err := x.f.store.ConversationMessages(ctx, conversation.ID)
			if err != nil || len(messages) != 1 || messages[0].ClientMessageID != op.Notifications[0].ContactID || messages[0].Text != op.WorkerFYI() || messages[0].Origin != domain.MessageOriginAutomation {
				t.Fatal("missing one canonical FYI", messages, err)
			}
			turn, err := x.f.store.TurnByID(ctx, messages[0].TurnID)
			if err != nil || turn.State != domain.TurnStateQueued || turn.HandledBySessionID != before.ID {
				t.Fatal("FYI dispatched instead of queued", turn, err)
			}
			after, _, err := x.f.store.GetSession(ctx, before.ID)
			if err != nil || after.ControllerOwner() != before.ControllerOwner() || after.ProvisionState != before.ProvisionState || (after.HibernatedAt == nil) != (before.HibernatedAt == nil) {
				t.Fatal("dormant FYI changed worker lifecycle", after, err)
			}
			if after.HibernatedAt != nil && !after.HibernatedAt.Equal(*before.HibernatedAt) {
				t.Fatal("hibernation marker changed")
			}
			op, err = x.service().Advance(ctx, op.ID)
			if err != nil || op.Phase != domain.ReplacementCompleted || op.NotificationCompletion == nil || op.NotificationCompletion.AcceptedCount != 1 {
				t.Fatal("positive dormant queue did not complete notification", op.Phase, err)
			}
		})
	}
}

func tuiNoticeFixture(t *testing.T) (*preparedExport, domain.SessionRecord, domain.OrchestratorReplacement, domain.ReplacementNoticeReceipt) {
	t.Helper()
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
	worker.ID, worker.IsTerminated, worker.Mode = "", false, domain.SessionModeTUI
	worker.Metadata.ProviderConversationID, worker.Metadata.ControllerGeneration = "", ""
	worker.Activity.State = domain.ActivityIdle
	worker.Metadata.RuntimeHandleID, worker.Metadata.RuntimeLaunchID = "worker-pane", "worker-launch"
	worker.Metadata.AgentSessionID, worker.Metadata.AgentSessionIDLaunchID = "worker-native", "worker-launch"
	worker, err = x.f.store.CreateSession(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	op := x.operation
	for range 3 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	claim, send, err := any(x.f.store).(noticeAttemptStore).BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, worker.ID, worker.ControllerOwner(), time.Now())
	if err != nil || !send {
		t.Fatal("missing TUI attempt", send, err)
	}
	worker, _, err = x.f.store.GetSession(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	return x, worker, op, claim
}

func TestReplacementDormantChatDoesNotReplayUnknownTUIWrite(t *testing.T) {
	x, worker, op, claim := tuiNoticeFixture(t)
	ctx := context.Background()
	changed, err := x.f.store.CommitSessionControllerEpoch(ctx, worker.ID, domain.SessionModeTUI, domain.SessionModeChat, worker.Metadata.AgentSessionID, time.Now())
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	worker, _, err = x.f.store.GetSession(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if changed, err := x.f.store.SetSessionHibernated(ctx, worker.ID, worker.Revision, &now); err != nil || !changed {
		t.Fatal(changed, err)
	}
	manager := sessionmanager.New(sessionmanager.Deps{Store: x.f.store.Store})
	if err := manager.DeliverReplacementWorkerFYI(ctx, op, worker.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.store.ConversationForSession(ctx, worker.ID); !errors.Is(err, domain.ErrNoConversation) {
		t.Fatal("unknown TUI write created a new Chat queue", err)
	}
	receipts, err := x.f.store.ReplacementWorkerNoticeReceipts(ctx, op.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != domain.ReplacementNoticeUncertain || receipts[0].AttemptID != claim.AttemptID {
		t.Fatal(receipts, err)
	}
}
