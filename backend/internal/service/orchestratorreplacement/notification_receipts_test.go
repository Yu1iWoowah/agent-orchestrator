package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type noticeAttemptStore interface {
	BeginReplacementWorkerNotice(context.Context, string, int64, domain.SessionID, domain.SessionControllerOwner, time.Time) (domain.ReplacementNoticeReceipt, bool, error)
	FinishReplacementWorkerNotice(context.Context, string, domain.SessionID, string, int64, bool, time.Time) (bool, error)
}

func TestReplacementTUIReceiptDoesNotRetryUncertainAttempt(t *testing.T) {
	for _, variant := range []string{"uncertain", "accepted", "wrong generation", "public contact", "wrong native", "unattempted"} {
		t.Run(variant, func(t *testing.T) {
			x := activationCoordinatorFixture(t)
			ctx := context.Background()
			api, ok := any(x.f.store).(noticeAttemptStore)
			if !ok {
				t.Fatal("managed notice attempt boundary unavailable")
			}
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
			worker.Metadata.RuntimeHandleID = "worker-pane"
			worker.Metadata.RuntimeLaunchID = "worker-launch"
			worker.Metadata.AgentSessionID = "worker-native"
			worker.Metadata.AgentSessionIDLaunchID = "worker-launch"
			worker, err = x.f.store.CreateSession(ctx, worker)
			if err != nil {
				t.Fatal(err)
			}
			persisted, _, err := x.f.store.GetSession(ctx, worker.ID)
			if err != nil || persisted.ControllerOwner() != worker.ControllerOwner() {
				t.Fatalf("worker fixture ownership mismatch: requested=%+v actual=%+v err=%v", worker.ControllerOwner(), persisted.ControllerOwner(), err)
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			claim, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, worker.ID, worker.ControllerOwner(), time.Now())
			if err != nil || !send || claim.AttemptID == "" || claim.State != domain.ReplacementNoticeUncertain || claim.OutcomeRecordedAt != nil {
				t.Fatal("missing pre-write intent", claim, send, err)
			}
			if _, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, worker.ID, worker.ControllerOwner(), time.Now()); err != nil || send {
				t.Fatal("second TUI caller repeated intent", send, err)
			}
			attempted := variant != "unattempted"
			if saved, err := api.FinishReplacementWorkerNotice(ctx, op.ID, worker.ID, claim.AttemptID, claim.Revision, attempted, time.Now()); err != nil || !saved {
				t.Fatal(saved, err)
			}
			if variant == "unattempted" {
				next, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, worker.ID, worker.ControllerOwner(), time.Now())
				if err != nil || !send || next.AttemptID == claim.AttemptID {
					t.Fatal("positive no-write could not retry", send, err)
				}
				if saved, err := api.FinishReplacementWorkerNotice(ctx, op.ID, worker.ID, claim.AttemptID, claim.Revision, false, time.Now()); err != nil || saved {
					t.Fatal("stale callback overwrote new intent", saved, err)
				}
				return
			}
			if _, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, worker.ID, worker.ControllerOwner(), time.Now()); err != nil || send {
				t.Fatal("uncertain TUI was blindly resent", send, err)
			}
			if variant != "uncertain" {
				worker, _, err = x.f.store.GetSession(ctx, worker.ID)
				if err != nil {
					t.Fatal(err)
				}
				worker.Metadata.ConversationCheckpointState = domain.ConversationCheckpointCoordination
				worker.Metadata.ConversationCheckpointGeneration = worker.Metadata.RuntimeLaunchID
				worker.Metadata.ConversationCheckpointNativeID = worker.Metadata.AgentSessionID
				worker.Metadata.ConversationCheckpointTurnID = "handoff-notice-attempt:" + claim.AttemptID
				if variant == "wrong generation" {
					worker.Metadata.ConversationCheckpointGeneration = "old-launch"
				}
				if variant == "public contact" {
					worker.Metadata.ConversationCheckpointTurnID = claim.ContactID
				}
				if variant == "wrong native" {
					worker.Metadata.ConversationCheckpointNativeID = "another-native"
				}
				if err := x.f.store.UpdateSession(ctx, worker); err != nil {
					t.Fatal(err)
				}
				actual, _, err := x.f.store.GetSession(ctx, worker.ID)
				if err != nil || actual.Metadata.ConversationCheckpointTurnID != worker.Metadata.ConversationCheckpointTurnID {
					t.Fatal("checkpoint fixture was not persisted", err)
				}
			}
			op, err = x.service().Advance(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if variant == "accepted" {
				if op.Phase != domain.ReplacementCompleted || op.NotificationCompletion == nil || op.NotificationCompletion.AcceptedCount != 1 {
					t.Fatal("positive scoped checkpoint was not reconciled", op.Phase)
				}
			} else if op.Phase != domain.ReplacementNotifying || op.NotificationCompletion != nil {
				t.Fatal("unverified TUI became complete", variant, op.Phase)
			}
		})
	}
}

func TestReplacementWorkerTerminatedAfterActivationGetsDurableSkip(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil || len(op.Notifications) != 1 || op.Phase != domain.ReplacementNotifying {
		t.Fatal("missing fixed notification target", op.Phase, err)
	}
	worker, found, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	worker.IsTerminated = true
	if err := x.f.store.UpdateSession(ctx, worker); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if op.Phase == domain.ReplacementCompleted {
			break
		}
	}
	if op.Phase != domain.ReplacementCompleted {
		t.Fatal("positive worker termination did not settle its notification", op.Phase)
	}
	if len(op.Notifications) != 1 || op.Notifications[0].SessionID != worker.ID {
		t.Fatal("completion erased the activation target", op.Notifications)
	}
	stored, found, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Completion *struct {
			TargetCount  int `json:"targetCount"`
			SkippedCount int `json:"skippedCount"`
		} `json:"notificationCompletion"`
	}
	if err := json.Unmarshal(raw, &view); err != nil || view.Completion == nil || view.Completion.TargetCount != 1 || view.Completion.SkippedCount != 1 {
		t.Fatal("durable notification completion proof missing", string(raw), err)
	}
}

func TestReplacementGenericSaveCannotMintNotificationCompletion(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	before := op.Revision
	op.Phase = domain.ReplacementCompleted
	op.NotificationCompletion = &domain.ReplacementNotificationCompletion{TargetCount: len(op.Notifications), AcceptedCount: len(op.Notifications), ReceiptHash: strings.Repeat("a", 64), CompletedAt: time.Now().UTC()}
	if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
		t.Fatal("generic caller minted trusted receipt", saved, err)
	}
	after, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || after.Revision != before || after.NotificationCompletion != nil || after.Phase != domain.ReplacementNotifying {
		t.Fatal("refused mint changed operation", after.Phase, err)
	}
}

func TestReplacementChatNoticeRequiresExactCanonicalAcceptance(t *testing.T) {
	for _, variant := range []string{"accepted", "wrong payload", "human", "wrong client", "other worker"} {
		t.Run(variant, func(t *testing.T) {
			x := activationCoordinatorFixture(t)
			ctx := context.Background()
			old, _, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			old.IsTerminated = true
			if err := x.f.store.UpdateSession(ctx, old); err != nil {
				t.Fatal(err)
			}
			worker, err := x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: x.operation.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{ControllerGeneration: "notice-worker", ProviderConversationID: "worker-native"}})
			if err != nil {
				t.Fatal(err)
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(op.Notifications) != 1 {
				t.Fatal("missing worker target", op.Notifications)
			}
			contact, text, origin := op.Notifications[0].ContactID, op.WorkerFYI(), domain.MessageOriginAutomation
			if variant == "wrong payload" {
				text += " altered"
			}
			if variant == "human" {
				origin = domain.MessageOriginHuman
			}
			if variant == "wrong client" {
				contact += "-another"
			}
			if variant == "other worker" {
				worker, err = x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{ControllerGeneration: "notice-other"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			conv, err := x.f.store.CreateConversation(ctx, "notice-conversation", domain.ConversationScopeSession, op.ProjectID, worker.ID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			_, err = x.f.store.AppendUserMessage(ctx, conv.ID, worker.ID, worker.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "notice-message", ClientMessageID: contact, Text: text, Origin: origin}, "notice-turn", time.Now())
			if err != nil {
				t.Fatal("canonical acceptance fixture", err)
			}
			op, err = x.service().Advance(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if variant == "accepted" {
				if op.Phase != domain.ReplacementCompleted || op.NotificationCompletion == nil || op.NotificationCompletion.AcceptedCount != 1 {
					t.Fatal("canonical queue acceptance not credited", op.Phase)
				}
				receipts, err := x.f.store.ReplacementWorkerNoticeReceipts(ctx, op.ID)
				if err != nil || len(receipts) != 1 || receipts[0].MessageID != "notice-message" || receipts[0].TurnID != "notice-turn" || receipts[0].State != domain.ReplacementNoticeAccepted {
					t.Fatal("missing durable queue identity", receipts, err)
				}
			} else if op.Phase != domain.ReplacementNotifying || op.NotificationCompletion != nil {
				t.Fatal("unrelated message became FYI evidence", variant, op.Phase)
			}
		})
	}
}

func TestReplacementNoticeBatchUsesOneRecoveryCheckpoint(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	for range 64 {
		_, err := x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: x.operation.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
	}
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(op.Notifications) != 65 || op.IntakeDrainRequestedAt == nil {
		t.Fatal("batch fixture not ready", len(op.Notifications))
	}
	full := false
	for i := 0; i < 1100; i++ {
		op, _, err = x.f.store.GetOrchestratorReplacement(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		op.Error = &domain.ReplacementFailure{Code: "QUOTA_FIXTURE", Message: fmt.Sprint(i)}
		saved, saveErr := x.f.store.SaveOrchestratorReplacement(ctx, op)
		if errors.Is(saveErr, domain.ErrReplacementRepairCapacity) {
			full = true
			break
		}
		if saveErr != nil || !saved {
			t.Fatal(saved, saveErr)
		}
	}
	if !full {
		t.Fatal("ordinary budget did not fill")
	}
	op, _, err = x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := op.Revision
	for _, target := range op.Notifications {
		worker, found, err := x.f.store.GetSession(ctx, target.SessionID)
		if err != nil || !found {
			t.Fatal(found, err)
		}
		worker.IsTerminated = true
		if err := x.f.store.UpdateSession(ctx, worker); err != nil {
			t.Fatal(err)
		}
	}
	op, err = x.service().Advance(ctx, op.ID)
	if err != nil || op.Phase != domain.ReplacementCompleted || op.Revision != before+1 || op.NotificationCompletion == nil || op.NotificationCompletion.SkippedCount != 65 || len(op.Notifications) != 65 {
		t.Fatal("batch consumed lifecycle reserve per worker", op.Phase, op.Revision, before, err)
	}
	for range 3 {
		again, err := x.service().Advance(ctx, op.ID)
		if err != nil || again.Revision != op.Revision {
			t.Fatal("settled poll wrote another checkpoint", again.Revision, err)
		}
	}
	raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var checkpoints, history int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM orchestrator_replacement_repair_snapshots WHERE operation_id=?`, op.ID).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM orchestrator_replacement_notice_history WHERE operation_id=?`, op.ID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 1025 || history != 65 {
		t.Fatal("unexpected checkpoint/history cardinality", checkpoints, history)
	}
	for _, statement := range []string{`UPDATE orchestrator_replacement_notices SET state_json=state_json WHERE operation_id=?`, `DELETE FROM orchestrator_replacement_notices WHERE operation_id=?`, `UPDATE orchestrator_replacement_notice_history SET state_json=state_json WHERE operation_id=?`, `DELETE FROM orchestrator_replacement_notice_history WHERE operation_id=?`} {
		if _, err := raw.Exec(statement, op.ID); err == nil {
			t.Fatal("settled history was mutable", statement)
		}
	}
}

func TestReplacementChatNoticeRetryKeepsCanonicalScopeAfterOwnerRotation(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x.f.store.CreateConversation(ctx, "notice-owner-queue", domain.ConversationScopeSession, x.operation.ProjectID, worker.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	api := any(x.f.store).(noticeAttemptStore)
	first, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, worker.ID, worker.ControllerOwner(), time.Now())
	if err != nil || !send {
		t.Fatal(first, send, err)
	}
	if _, err := api.FinishReplacementWorkerNotice(ctx, op.ID, worker.ID, first.AttemptID, first.Revision, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	worker.Metadata.ControllerGeneration = "worker-next-generation"
	if err := x.f.store.UpdateSession(ctx, worker); err != nil {
		t.Fatal(err)
	}
	next, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, worker.ID, worker.ControllerOwner(), time.Now())
	if err != nil || !send || next.Owner != worker.ControllerOwner() || next.ConversationID != c.ID || next.ContactID != first.ContactID {
		t.Fatal("same canonical queue could not recover changed owner", next, send, err)
	}
	if _, err := x.f.store.AppendUserMessage(ctx, c.ID, worker.ID, worker.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "owner-notice-message", ClientMessageID: next.ContactID, Text: op.WorkerFYI(), Origin: domain.MessageOriginAutomation}, "owner-notice-turn", time.Now()); err != nil {
		t.Fatal(err)
	}
	op, err = x.service().Advance(ctx, op.ID)
	if err != nil || op.Phase != domain.ReplacementCompleted {
		t.Fatal(op.Phase, err)
	}
}

func TestReplacementGenericSaveCannotAppendNotificationTarget(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	op.Notifications = append(op.Notifications, domain.ReplacementWorkerNotification{SessionID: worker.ID, ContactID: "handoff-notice:" + op.ID + ":" + string(worker.ID), State: "pending", CreatedAt: time.Now()})
	if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
		t.Fatal("generic save changed immutable target set", saved, err)
	}
}

func TestReplacementNoticeRetryBudgetCannotStarveTerminalReceipts(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.store.CreateConversation(ctx, "notice-budget-queue", domain.ConversationScopeSession, x.operation.ProjectID, worker.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
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
			t.Fatal("invalid retry fixture", send, err)
		}
		if _, err := api.FinishReplacementWorkerNotice(ctx, op.ID, worker.ID, claim.AttemptID, claim.Revision, false, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if !exhausted {
		t.Fatal("unbounded retry history")
	}
	notices, err := x.f.store.ReplacementWorkerNoticeReceipts(ctx, op.ID)
	if err != nil || len(notices) != 1 || notices[0].ErrorCode != "DELIVERY_RETRY_CAPACITY" {
		t.Fatal("retry exhaustion is not operator visible", notices, err)
	}
	worker.IsTerminated = true
	if err := x.f.store.UpdateSession(ctx, worker); err != nil {
		t.Fatal(err)
	}
	op, err = x.service().Advance(ctx, op.ID)
	if err != nil || op.Phase != domain.ReplacementCompleted || op.NotificationCompletion.SkippedCount != 1 {
		t.Fatal("retry churn starved terminal receipt", op.Phase, err)
	}
}

func TestReplacementNoticeCorruptionCannotBecomePositiveReceipt(t *testing.T) {
	for _, path := range []string{"$.attemptId", "$.unexpectedFact"} {
		t.Run(path, func(t *testing.T) {
			x := activationCoordinatorFixture(t)
			ctx := context.Background()
			worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := x.f.store.CreateConversation(ctx, "corrupt-notice-queue", domain.ConversationScopeSession, x.operation.ProjectID, worker.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			api := any(x.f.store).(noticeAttemptStore)
			_, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, worker.ID, worker.ControllerOwner(), time.Now())
			if err != nil || !send {
				t.Fatal(send, err)
			}
			raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(`UPDATE orchestrator_replacement_notices SET state_json=json_set(state_json,?,'unverified') WHERE operation_id=?`, path, op.ID); err != nil {
				t.Fatal(err)
			}
			worker.IsTerminated = true
			if err := x.f.store.UpdateSession(ctx, worker); err != nil {
				t.Fatal(err)
			}
			if _, err := x.service().Advance(ctx, op.ID); err == nil {
				t.Fatal("unverified projection became skipped/accepted")
			}
			current, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil || current.Phase != domain.ReplacementNotifying || current.Revision != op.Revision || current.NotificationCompletion != nil {
				t.Fatal("corrupt notice changed lifecycle", current.Phase, err)
			}
			fenced, err := x.f.store.ReplacementWakeFenced(ctx, op.SuccessorSessionID)
			if err != nil || fenced {
				t.Fatal("receipt corruption re-fenced valid successor", fenced, err)
			}
		})
	}
}

func TestReplacementWorkerSeedRollbackRetainsPositiveTerminationEvidence(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	original, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	original.IsTerminated = true
	if err := x.f.store.UpdateSession(ctx, original); err != nil {
		t.Fatal(err)
	}
	seed, err := x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: x.operation.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, ProvisionState: domain.SessionProvisionProvisioning, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Notifications) != 1 || op.Notifications[0].SessionID != seed.ID {
		t.Fatal("seed not captured by activation", op.Notifications)
	}
	deleted, err := x.f.store.DeleteSession(ctx, seed.ID)
	if err != nil || deleted {
		t.Fatal("seed rollback erased immutable notification target", deleted, err)
	}
	retained, found, err := x.f.store.GetSession(ctx, seed.ID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	// The normal rollback fallback marks an undeletable seed terminated. Retain
	// that durable fact rather than treating an absent row as proof of death.
	retained.IsTerminated = true
	if err := x.f.store.UpdateSession(ctx, retained); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if op.Phase == domain.ReplacementCompleted {
			break
		}
	}
	if op.Phase != domain.ReplacementCompleted || op.NotificationCompletion.SkippedCount != 1 {
		t.Fatal("verified seed cleanup did not settle", op.Phase)
	}
}

func TestReplacementNoticeLedgerRejectsFifthReceiptState(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.store.CreateConversation(ctx, "four-state-notice", domain.ConversationScopeSession, x.operation.ProjectID, worker.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	api := any(x.f.store).(noticeAttemptStore)
	if _, send, err := api.BeginReplacementWorkerNotice(ctx, op.ID, op.Revision, worker.ID, worker.ControllerOwner(), time.Now()); err != nil || !send {
		t.Fatal(send, err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE orchestrator_replacement_notices SET state='sending',state_json=json_set(state_json,'$.state','sending') WHERE operation_id=?`, op.ID); err == nil {
		t.Fatal("ledger accepted a fifth delivery evidence state")
	}
}

func TestReplacementNoticePublicationRollbackRecoversCanonicalAcceptance(t *testing.T) {
	for _, stage := range []string{"history", "completion"} {
		t.Run(stage, func(t *testing.T) {
			x := activationCoordinatorFixture(t)
			ctx := context.Background()
			worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			c, err := x.f.store.CreateConversation(ctx, "notice-rollback-queue", domain.ConversationScopeSession, x.operation.ProjectID, worker.ID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				op, err = x.service().Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := x.f.store.AppendUserMessage(ctx, c.ID, worker.ID, worker.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "rollback-notice-message", ClientMessageID: op.Notifications[0].ContactID, Text: op.WorkerFYI(), Origin: domain.MessageOriginAutomation}, "rollback-notice-turn", time.Now()); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			fault := `CREATE TRIGGER notice_enospc BEFORE INSERT ON orchestrator_replacement_notice_history BEGIN SELECT RAISE(ABORT,'fixture ENOSPC'); END`
			if stage == "completion" {
				fault = `CREATE TRIGGER notice_enospc BEFORE UPDATE ON orchestrator_replacements WHEN json_extract(NEW.state_json,'$.notificationCompletion') IS NOT NULL BEGIN SELECT RAISE(ABORT,'fixture ENOSPC'); END`
			}
			if _, err := raw.Exec(fault); err != nil {
				t.Fatal(err)
			}
			if _, err := x.service().Advance(ctx, op.ID); err == nil {
				t.Fatal("publication fault was ignored")
			}
			current, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil || current.Revision != op.Revision || current.Phase != domain.ReplacementNotifying || current.NotificationCompletion != nil {
				t.Fatal("partial completion persisted", current.Phase, err)
			}
			var count int
			if err := raw.QueryRow(`SELECT COUNT(*) FROM orchestrator_replacement_notice_history WHERE operation_id=?`, op.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial history persisted", count, err)
			}
			if _, err := raw.Exec(`DROP TRIGGER notice_enospc`); err != nil {
				t.Fatal(err)
			}
			current, err = x.service().Advance(ctx, op.ID)
			if err != nil || current.Phase != domain.ReplacementCompleted || current.NotificationCompletion.AcceptedCount != 1 {
				t.Fatal("canonical receipt could not recover", current.Phase, err)
			}
			if err := raw.QueryRow(`SELECT COUNT(*) FROM conversation_messages WHERE client_message_id=?`, op.Notifications[0].ContactID).Scan(&count); err != nil || count != 1 {
				t.Fatal("recovery duplicated queue message", count, err)
			}
		})
	}
}
