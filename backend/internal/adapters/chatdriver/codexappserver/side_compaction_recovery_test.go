package codexappserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type openedSideDriver struct {
	ports.ChatDriver
	conv ports.ChatConversation
	main ports.ChatConversation
}

func (d openedSideDriver) Probe(context.Context) (ports.ChatCapabilities, error) {
	return capabilities(), nil
}

func (d openedSideDriver) Start(context.Context, ports.ChatStartConfig) (ports.ChatConversation, error) {
	return d.main, nil
}

func (d openedSideDriver) Resume(context.Context, ports.ChatResumeConfig) (ports.ChatConversation, error) {
	return d.conv, nil
}

func TestSideResumesQueueAfterRealCodexCompactionTimeout(t *testing.T) {
	ctx := t.Context()
	st := sqlitetest.MustOpenAt(t, t.TempDir())
	at := time.Now().UTC()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "compact", Path: t.TempDir(), RegisteredAt: at}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.CreateSession(ctx, domain.SessionRecord{ID: "compact-session", ProjectID: "compact", Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Kind: domain.KindWorker, CreatedAt: at, UpdatedAt: at})
	if err != nil {
		t.Fatal(err)
	}

	driver, server := newTestDriver(t)
	env := map[string]string{"CODEX_HOME": t.TempDir()}
	opened, err := driver.Start(ctx, ports.ChatStartConfig{WorkspacePath: t.TempDir(), Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	mainDriver, _ := newTestDriver(t)
	main, err := mainDriver.Start(ctx, ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = main.Close() })
	svc := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Drivers: handoffDriverRegistry{driver: openedSideDriver{ChatDriver: driver, conv: opened, main: main}}, NewID: uuid.NewString, AppRunID: "compact-live"})
	t.Cleanup(func() { _ = svc.CloseIndependentSideChat(context.Background(), rec.ID, "side") })
	parent, err := svc.Start(ctx, chatsvc.StartConfig{SessionID: rec.ID, ProjectID: rec.ProjectID, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background(), rec.ID) })
	config, err := json.Marshal(map[string]any{"WorkspacePath": t.TempDir(), "Env": env})
	if err != nil {
		t.Fatal(err)
	}
	unverified := false
	side := domain.SideConversation{ID: "side", PolicyVersion: 1, SessionID: rec.ID, MainConversationID: parent.ConversationID(), State: "ready", Generation: "g", Harness: domain.HarnessCodex}
	records := []chatsvc.SideRecoveryRecord{{Side: side, PolicyVersion: 1, Generation: "g", Harness: domain.HarnessCodex, ProviderHostID: "btw-compact-live-side", ProviderForkID: "thread-1", LaunchConfig: config, BoundaryState: "delivered", OutboundUnverified: &unverified}}
	if err := svc.ClaimSideChatLaunch(ctx, "compact-live"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverSideChatLaunch(ctx, "compact-live", records); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot, err := svc.SideSnapshot(ctx, rec.ID, "side", time.Time{}, 100)
		if err == nil && snapshot.Side.State == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("side did not attach: %+v %v", snapshot, err)
		}
		time.Sleep(time.Millisecond)
	}
	compactCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := svc.CompactSideChat(compactCtx, rec.ID, "side"); done <- err }()
	server.awaitFrame(func(f frame) bool { return f.Method == "thread/compact/start" })
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ports.ErrChatCompactionNotSent) {
		t.Fatalf("forwarded compact misclassified: %v", err)
	}
	page, err := st.ListAttentionQuestions(ctx, rec.ID, domain.AttentionQuestionQuery{}, time.Now().UTC())
	if err != nil || page.Open != 1 {
		t.Fatalf("unknown compaction invisible: %+v %v", page, err)
	}
	queued, err := svc.SendSideQuestion(ctx, rec.ID, "side", ports.ChatUserMessage{Text: "next question", ClientMessageID: "next"})
	if err != nil {
		t.Fatal(err)
	}
	server.push(`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"original-compact","item":{"type":"contextCompaction","id":"compact-item"}}}`)
	server.push(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"original-compact","status":"completed"}}}`)
	server.awaitFrame(func(f frame) bool { return f.Method == "turn/start" })
	server.push(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}`)
	deadline = time.Now().Add(3 * time.Second)
	for {
		snapshot, err := svc.SideSnapshot(ctx, rec.ID, "side", time.Time{}, 100)
		if err == nil && len(snapshot.Turns) == 1 && snapshot.Turns[0].ID == queued.ID && snapshot.Turns[0].State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("original completion did not resume queue: %+v %v", snapshot, err)
		}
		time.Sleep(time.Millisecond)
	}
	page, err = st.ListAttentionQuestions(ctx, rec.ID, domain.AttentionQuestionQuery{}, time.Now().UTC())
	if err != nil || page.Open != 0 {
		t.Fatalf("completed compaction question stayed open: %+v %v", page, err)
	}
}
