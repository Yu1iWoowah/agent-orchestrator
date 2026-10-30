package orchestratorreplacement_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
)

type noticeRuntime struct {
	replacement.Runtime
	deliver func(context.Context, domain.OrchestratorReplacement, domain.SessionID) error
}

func (r noticeRuntime) DeliverReplacementWorkerFYI(ctx context.Context, op domain.OrchestratorReplacement, id domain.SessionID) error {
	return r.deliver(ctx, op, id)
}

func TestReplacementNotificationsRunOutsideProjectGateAndRecoverLostReply(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed", true: "lost reply after acceptance"}[accepted], func(t *testing.T) {
			x := activationCoordinatorFixture(t)
			ctx := context.Background()
			held, calls := false, 0
			runtime := noticeRuntime{Runtime: x.f.runtime, deliver: func(ctx context.Context, op domain.OrchestratorReplacement, id domain.SessionID) error {
				calls++
				if held {
					t.Fatal("notification transport holds the project gate")
				}
				if id != x.worker.ID {
					t.Fatal("wrong target", id)
				}
				if accepted {
					c, err := x.f.store.CreateConversation(ctx, "notice-delivery", domain.ConversationScopeSession, op.ProjectID, id, time.Now())
					if err != nil {
						t.Fatal(err)
					}
					if _, err := x.f.store.AppendUserMessage(ctx, c.ID, id, x.worker.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "notice-message", ClientMessageID: op.Notifications[0].ContactID, Text: op.WorkerFYI(), Origin: domain.MessageOriginAutomation}, "notice-turn", time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("transport reply lost")
			}}
			svc := replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: runtime, Documents: x.documents, NativeTranscripts: x.reader, WithProjectGate: func(ctx context.Context, _ domain.ProjectID, run func(context.Context) error) error {
				held = true
				defer func() { held = false }()
				return run(ctx)
			}})
			op := x.operation
			for range 4 {
				var err error
				op, err = svc.Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				if calls > 0 {
					break
				}
			}
			if calls != 1 || op.IntakeReleasedAt == nil || op.IntakeDrainRequestedAt == nil || op.Error != nil {
				t.Fatal("notice did not preserve released intake", calls, op.Phase, op.Error)
			}
			expected := domain.ReplacementNotifying
			if accepted {
				expected = domain.ReplacementCompleted
			}
			if op.Phase != expected {
				t.Fatal("wrong durable result", op.Phase, expected)
			}
			fenced, err := x.f.store.ReplacementWakeFenced(ctx, op.SuccessorSessionID)
			if err != nil || fenced {
				t.Fatal("notice failure re-fenced successor", fenced, err)
			}
			if accepted {
				if _, err := svc.Advance(ctx, op.ID); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatal("completed notice resent", calls)
				}
			}
		})
	}
}

func TestReplacementNotificationTransportHasBoundedContext(t *testing.T) {
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
	calls := 0
	runtime := noticeRuntime{Runtime: x.f.runtime, deliver: func(ctx context.Context, _ domain.OrchestratorReplacement, _ domain.SessionID) error {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("unbounded notification transport")
		}
		return nil
	}}
	svc := replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: runtime, Documents: x.documents, NativeTranscripts: x.reader})
	if _, err := svc.Advance(ctx, op.ID); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
}

func TestReplacementNotificationCancellationDoesNotStarveTargets(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	for range 31 {
		if _, err := x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: x.operation.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
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
	seen := map[domain.SessionID]bool{}
	var cancel context.CancelFunc
	runtime := noticeRuntime{Runtime: x.f.runtime, deliver: func(_ context.Context, _ domain.OrchestratorReplacement, id domain.SessionID) error {
		seen[id] = true
		cancel()
		return context.Canceled
	}}
	svc := replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: runtime, Documents: x.documents, NativeTranscripts: x.reader})
	for range len(op.Notifications) {
		var attempt context.Context
		attempt, cancel = context.WithCancel(ctx)
		_, err := svc.Advance(attempt, op.ID)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatal("missing cancellation", err)
		}
	}
	if len(seen) != len(op.Notifications) {
		t.Fatal("cancelled batches starved targets", len(seen), len(op.Notifications))
	}
	current, err := svc.Get(ctx, op.ID)
	if err != nil || current.Phase != domain.ReplacementNotifying || current.Revision != op.Revision {
		t.Fatal("notification cancellation changed lifecycle", current.Phase, err)
	}
}

type noticeReportDelivery struct {
	targets    []domain.SessionID
	messages   []string
	interrupts int
}

func (d *noticeReportDelivery) Submit(_ context.Context, target domain.SessionID, message string, _ string) error {
	d.targets = append(d.targets, target)
	d.messages = append(d.messages, message)
	return nil
}
func (d *noticeReportDelivery) Interrupt(context.Context, domain.SessionID) error {
	d.interrupts++
	return nil
}

func TestReplacementFYIPreservesOpenReportsCutoffAndWorkerProgress(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	reports := reportsvc.New(reportsvc.Deps{Store: x.f.store.Store})
	note := "Q1、Q3（source=" + string(x.worker.ID) + "）未答；next D-9；last ACK D-7；D-8 尚待 ACK"
	report, err := reports.Create(ctx, reportsvc.CreateInput{SessionID: x.worker.ID, State: domain.ReportNeedsInput, Note: note})
	if err != nil {
		t.Fatal(err)
	}
	beforeWorker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, path := range []string{x.nativePath, filepath.Join(x.operation.Transcript.Receipt.Directory, "handoff.md"), filepath.Join(x.operation.Transcript.Receipt.Directory, "transcript.md"), filepath.Join(x.operation.Transcript.Receipt.Directory, "manifest.json")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = data
	}
	runtime := noticeRuntime{Runtime: x.f.runtime, deliver: func(ctx context.Context, op domain.OrchestratorReplacement, id domain.SessionID) error {
		c, err := x.f.store.CreateConversation(ctx, "report-worker-notice", domain.ConversationScopeSession, op.ProjectID, id, time.Now())
		if err != nil {
			return err
		}
		_, err = x.f.store.AppendUserMessage(ctx, c.ID, id, x.worker.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "report-worker-notice-message", ClientMessageID: op.Notifications[0].ContactID, Text: op.WorkerFYI(), Origin: domain.MessageOriginAutomation}, "report-worker-notice-turn", time.Now())
		return err
	}}
	svc := replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: runtime, Documents: x.documents, NativeTranscripts: x.reader})
	op := x.operation
	for range 6 {
		op, err = svc.Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if op.Phase == domain.ReplacementCompleted {
			break
		}
	}
	if op.Phase != domain.ReplacementCompleted || op.NotificationCompletion == nil || op.NotificationCompletion.AcceptedCount != 1 {
		t.Fatal(op.Phase, op.Error)
	}
	retained, found, err := x.f.store.GetReport(ctx, report.ID)
	if err != nil || !found || retained.Note != note || retained.SessionID != x.worker.ID || retained.State != domain.ReportNeedsInput || retained.DeliveryState != domain.ReportPending {
		t.Fatal("FYI acknowledged or changed source-scoped questions", retained, err)
	}
	afterWorker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil || afterWorker.ControllerOwner() != beforeWorker.ControllerOwner() || afterWorker.Activity != beforeWorker.Activity || afterWorker.IsTerminated || *x.actualStops != 1 {
		t.Fatal("FYI changed worker/source runtime", afterWorker, err)
	}
	for path, before := range files {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("FYI changed immutable source/evidence", path, err)
		}
	}
	if !reflect.DeepEqual(op.Transcript, x.operation.Transcript) {
		t.Fatal("FYI rewrote frozen transcript cutoff")
	}
	delivery := &noticeReportDelivery{}
	router := reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: x.f.store.Store, Delivery: delivery})
	if _, err := router.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if len(delivery.targets) != 1 || delivery.targets[0] != op.SuccessorSessionID || !strings.Contains(delivery.messages[0], note) || delivery.interrupts != 0 {
		t.Fatal("pending report did not route unchanged to current owner", delivery)
	}
}

func TestReplacementNotificationRestartAdvancesPastSettledTargets(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	for range 31 {
		if _, err := x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: x.operation.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{ControllerGeneration: "worker-generation"}}); err != nil {
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
	calls := 0
	runtime := noticeRuntime{Runtime: x.f.runtime, deliver: func(ctx context.Context, op domain.OrchestratorReplacement, id domain.SessionID) error {
		calls++
		worker, _, err := x.f.store.GetSession(ctx, id)
		if err != nil {
			return err
		}
		c, err := x.f.store.CreateConversation(ctx, "batch-notice-"+string(id), domain.ConversationScopeSession, op.ProjectID, id, time.Now())
		if err != nil {
			return err
		}
		var contact string
		for _, n := range op.Notifications {
			if n.SessionID == id {
				contact = n.ContactID
			}
		}
		_, err = x.f.store.AppendUserMessage(ctx, c.ID, id, worker.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "batch-message-" + string(id), ClientMessageID: contact, Text: op.WorkerFYI(), Origin: domain.MessageOriginAutomation}, "batch-turn-"+string(id), time.Now())
		return err
	}}
	for range 2 {
		// A new coordinator per batch simulates losing all process-local scheduling.
		svc := replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: runtime, Documents: x.documents, NativeTranscripts: x.reader})
		op, err = svc.Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if op.Phase != domain.ReplacementCompleted || calls != 32 {
		t.Fatal("restart revisited settled targets instead of pending work", op.Phase, calls)
	}
}
