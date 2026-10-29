package chat_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type observedConversation struct {
	*fakeConversation
	observed time.Time
	nativeID atomic.Value
}

func (c *observedConversation) ProviderConversationID() string {
	if id := c.nativeID.Load(); id != nil {
		return id.(string)
	}
	return c.fakeConversation.ProviderConversationID()
}

func (c *observedConversation) AgentSettingsObservation() domain.AgentSettingsEffective {
	at := c.observed
	return domain.AgentSettingsEffective{Model: domain.AgentSettingObservation{State: domain.AgentSettingKnown, Value: "provider-model"}, FastMode: domain.AgentSettingObservation{State: domain.AgentSettingNotProvided, Reason: "provider_not_offered"}, ObservedAt: &at, Source: "provider_config"}
}

func TestLiveSettingsReceiptsAndDispatchSnapshotAreFenced(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	if err := st.BindSessionAgentAccount(ctx, testSession, domain.AgentAccountProviderCodex, "device-codex", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionLaunchOptions(ctx, testSession, domain.AgentLaunchOptions{FastMode: true}); err != nil {
		t.Fatal(err)
	}
	conv := &observedConversation{fakeConversation: newFakeConversation(), observed: time.Now().UTC()}
	sent := make(chan struct{}, 1)
	conv.onSend = func(string) { sent <- struct{}{} }
	svc := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Reader: fullSnapshotReader(st), Drivers: fakeRegistry{driver: fakeDriver{conv: conv}}, Log: slog.New(slog.DiscardHandler), NewID: uuid.NewString})
	if _, err := svc.Start(ctx, chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(), FastMode: domain.AgentFastOn}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(ctx, testSession) })
	// This fixture starts Chat directly, without session_manager.ControllerReady.
	// Publish the native identity that the real startup callback commits.
	record, found, err := st.GetSession(ctx, testSession)
	if err != nil || !found {
		t.Fatalf("session: %v %v", found, err)
	}
	record.Metadata.ProviderConversationID = conv.ProviderConversationID()
	if err := st.UpdateSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	source, ok := any(svc).(ports.AgentSettingsObservationSource)
	if !ok {
		t.Fatal("Chat does not expose non-waking provider receipts")
	}
	reader := agentsettings.New(st, time.Now)
	reader.SetObservationSource(source)
	got, err := reader.Read(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effective.FastMode.State != domain.AgentSettingNotProvided || got.Effective.Model.Value != "provider-model" {
		t.Fatalf("requested preference replaced provider receipts: %+v", got.Effective)
	}
	if _, err := svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("turn not dispatched")
	}
	if _, err := svc.SetTurnSettings(ctx, testSession, domain.ConversationSettings{FastMode: false}); err != nil {
		t.Fatal(err)
	}
	got, err = reader.Read(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentTurn == nil || got.CurrentTurn.Settings == nil || got.CurrentTurn.Settings.FastMode != domain.AgentFastOn {
		t.Fatalf("current turn snapshot changed with next-turn preference: %+v", got.CurrentTurn)
	}
	conv.nativeID.Store("uncommitted-provider-branch")
	got, err = reader.Read(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effective.Model.State != domain.AgentSettingUnknown || got.CurrentTurn != nil {
		t.Fatalf("uncommitted provider branch borrowed old owner: %+v", got)
	}
	conv.nativeID.Store("thread-1")
	if err := st.BindSessionAgentAccount(ctx, testSession, domain.AgentAccountProviderCodex, "managed-replacement", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = reader.Read(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effective.Model.State != domain.AgentSettingUnknown || got.Effective.Model.Value != "" || got.CurrentTurn != nil {
		t.Fatalf("old account evidence leaked: %+v", got)
	}
	if err := svc.Stop(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	got, err = reader.Read(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effective.FastMode.State != domain.AgentSettingUnknown || got.CurrentTurn != nil {
		t.Fatalf("stopped controller was woken or trusted: %+v", got)
	}
}
