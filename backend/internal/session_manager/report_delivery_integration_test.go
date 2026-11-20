package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type reportAckFailureStore struct {
	*sqlite.Store
	err error
}

func (s reportAckFailureStore) AcknowledgeReportBatch(context.Context, domain.ProjectID, string, time.Time) (int, error) {
	return 0, s.err
}

type chatReportDelivery struct {
	manager   *Manager
	submitErr error
}

func (d chatReportDelivery) Submit(ctx context.Context, id domain.SessionID, body, key string) error {
	if d.submitErr != nil {
		return d.submitErr
	}
	return d.manager.SendSemantic(ctx, id, body, key)
}

func (d chatReportDelivery) Interrupt(ctx context.Context, id domain.SessionID) error {
	return d.manager.InterruptTUI(ctx, id)
}

func TestReportBatchRetryAfterChatAcceptanceAndRestart(t *testing.T) {
	ctx := t.Context()
	dataDir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dataDir)
	now := time.Now().UTC()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: string(chatTestProject), Path: dataDir, Config: testRoleAgents(), RegisteredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	orch, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: chatTestProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex, DisplayName: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	newChat := func(nativeID string) (*chatsvc.Service, *Manager) {
		t.Helper()
		startProvider := func() ports.ChatConversation { return newIntegrationChatConversation("provider-thread") }
		chat := chatsvc.New(chatsvc.Options{
			Store: st, Sessions: st, NewID: uuid.NewString,
			Drivers: integrationChatRegistry{domain.HarnessCodex: integrationChatDriver{
				harness: domain.HarnessCodex, start: startProvider, resume: startProvider,
			}},
		})
		t.Cleanup(func() { _ = chat.StopAll(context.Background()) })
		if _, err := chat.StartChat(ctx, chatsvc.StartConfig{
			SessionID: orch.ID, ProjectID: chatTestProject, Kind: orch.Kind, Harness: orch.Harness,
			DataDir: dataDir, WorkspacePath: dataDir, ProviderConversationID: nativeID,
		}); err != nil {
			t.Fatalf("start Chat: %v", err)
		}
		return chat, New(Deps{Store: st, Chat: integrationChatLauncher{service: chat}, DataDir: dataDir})
	}
	addReport := func(id, message string) {
		t.Helper()
		if _, err := st.CreateReport(ctx, domain.ReportRecord{
			ID: id, SessionID: worker.ID, ProjectID: chatTestProject, Message: message,
			CreatedAt: now, AvailableAt: now, RepeatCount: 1, DeliveryState: domain.ReportPending,
		}); err != nil {
			t.Fatal(err)
		}
	}

	firstChat, firstManager := newChat("")
	delivery := &chatReportDelivery{manager: firstManager, submitErr: reportsvc.ErrUnacknowledgedDelivery}
	ackErr := errors.New("daemon exited before acknowledging the accepted batch")
	coordinator := reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{
		Store: reportAckFailureStore{st, ackErr}, Delivery: delivery, Now: func() time.Time { return now }, MaxDeliveryAttempts: 1,
	})
	addReport("earlier-failure", "could not get through")
	if _, err := coordinator.RunDue(ctx); !errors.Is(err, reportsvc.ErrUnacknowledgedDelivery) {
		t.Fatalf("first batch = %v, want failed delivery", err)
	}
	if got := coordinator.UndeliveredCount(chatTestProject); got != 1 {
		t.Fatalf("failed reports = %d, want 1", got)
	}
	delivery.submitErr = nil
	addReport("accepted", "finished the work")
	if _, err := coordinator.RunDue(ctx); !errors.Is(err, ackErr) {
		t.Fatalf("second batch = %v, want post-acceptance ack error", err)
	}
	conv, err := st.ConversationForSession(ctx, orch.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.LoadConversationSnapshot(ctx, conv.ID)
	if err != nil || len(before.Messages) != 1 ||
		before.Messages[0].ClientMessageID != "report-batch:accepted" ||
		!strings.Contains(before.Messages[0].Text, "Note: 1 report could not be delivered.") {
		t.Fatalf("Chat before restart = %+v, err = %v", before.Messages, err)
	}
	if err := firstChat.Stop(ctx, orch.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RequeueClaimedReports(ctx); err != nil || n != 1 {
		t.Fatalf("recover claims = %d, err = %v", n, err)
	}
	_, restartedManager := newChat("provider-thread")
	restarted := reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{
		Store: st, Delivery: chatReportDelivery{manager: restartedManager}, Now: func() time.Time { return now },
	})
	now = now.Add(reportsvc.OrchestratorBusyRetry)
	if _, err := restarted.RunDue(ctx); err != nil {
		t.Fatalf("retry same batch after restart: %v", err)
	}
	after, err := st.LoadConversationSnapshot(ctx, conv.ID)
	if err != nil || len(after.Messages) != 1 || after.Messages[0].ID != before.Messages[0].ID || after.Messages[0].Text != before.Messages[0].Text {
		t.Fatalf("Chat after retry = %+v, err = %v", after.Messages, err)
	}
	report, ok, err := st.GetReport(ctx, "accepted")
	if err != nil || !ok || report.DeliveryState != domain.ReportAcknowledged || report.DeliveryAttempts != 2 {
		t.Fatalf("retried report = %+v, found = %v, err = %v", report, ok, err)
	}
}
