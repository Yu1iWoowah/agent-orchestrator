package chat_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestConversationStoreBooleanFastChoiceReplacesPaidPreference(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	conversation, err := st.CreateConversation(ctx, "fast-choice", domain.ConversationScopeProject, testProject, testSession, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, fast := range []bool{true, false} {
		if err := st.SetConversationSettings(ctx, conversation.ID, domain.ConversationSettings{FastMode: fast}, time.Now()); err != nil {
			t.Fatal(err)
		}
		stored, err := st.ConversationForSession(ctx, testSession)
		want := domain.AgentFastOff
		if fast {
			want = domain.AgentFastOn
		}
		if err != nil || stored.Settings.FastMode != fast || stored.Settings.AgentFastMode != want {
			t.Fatalf("fast=%v stored=%+v err=%v", fast, stored.Settings, err)
		}
	}
}

func TestChatFastOffPersistsAcrossRestartAndHibernatedSnapshot(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	var resumed ports.ChatResumeConfig
	makeService := func() *chatsvc.Service {
		return chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Reader: fullSnapshotReader(st), Drivers: fakeRegistry{driver: fakeDriver{conv: newFakeConversation(), resumeCfg: &resumed}}, Log: slog.New(slog.DiscardHandler), NewID: uuid.NewString})
	}
	svc := makeService()
	cfg := chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(), FastMode: domain.AgentFastOn}
	if _, err := svc.Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(ctx, testSession) })
	settings, err := svc.SetTurnSettings(ctx, testSession, domain.ConversationSettings{FastMode: false})
	if err != nil {
		t.Fatal(err)
	}
	if settings.FastMode || settings.AgentFastMode != domain.AgentFastOff {
		t.Errorf("live fast-off choice is inconsistent: %+v", settings)
	}
	stored, err := st.ConversationForSession(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Settings.FastMode || stored.Settings.AgentFastMode == domain.AgentFastOn {
		t.Errorf("fast-off choice was not persisted: %+v", stored.Settings)
	}
	if err := svc.Stop(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	rec, found, err := st.GetSession(ctx, testSession)
	if err != nil || !found {
		t.Fatalf("session found=%v err=%v", found, err)
	}
	now := time.Now()
	if changed, err := st.SetSessionHibernated(ctx, testSession, rec.Revision, &now); err != nil || !changed {
		t.Fatalf("hibernate: changed=%v err=%v", changed, err)
	}
	replacement := makeService()
	snapshot, err := replacement.Snapshot(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Conversation.Settings.FastMode || snapshot.Conversation.Settings.AgentFastMode == domain.AgentFastOn {
		t.Errorf("hibernated snapshot re-enabled fast: %+v", snapshot.Conversation.Settings)
	}
	rec, _, _ = st.GetSession(ctx, testSession)
	if changed, err := st.SetSessionHibernated(ctx, testSession, rec.Revision, nil); err != nil || !changed {
		t.Fatalf("wake: changed=%v err=%v", changed, err)
	}
	cfg.ProviderConversationID = "thread-1"
	if _, err := replacement.Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Stop(ctx, testSession) })
	if resumed.FastMode == domain.AgentFastOn || resumed.FastMode == "" {
		t.Errorf("restart re-enabled fast or lost explicit off: %+v", resumed)
	}
}

func TestSpawnChatFastSurvivesControllerRestore(t *testing.T) {
	for _, mode := range []domain.AgentFastMode{domain.AgentFastOn, domain.AgentFastOff, domain.AgentFastDefault} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			var started ports.ChatStartConfig
			var resumed ports.ChatResumeConfig
			makeService := func() *chatsvc.Service {
				return chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Reader: fullSnapshotReader(st), Drivers: fakeRegistry{driver: fakeDriver{conv: newFakeConversation(), startCfg: &started, resumeCfg: &resumed}}, Log: slog.New(slog.DiscardHandler), NewID: uuid.NewString})
			}
			svc := makeService()
			cfg := chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(), FastMode: mode}
			if _, err := svc.Start(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			if started.FastMode != mode {
				t.Fatalf("driver start fast=%q want=%q", started.FastMode, mode)
			}
			stored, err := st.ConversationForSession(ctx, testSession)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Settings.FastMode != (mode == domain.AgentFastOn) {
				t.Fatalf("stored=%+v", stored.Settings)
			}
			if err = svc.Stop(ctx, testSession); err != nil {
				t.Fatal(err)
			}
			cfg.ProviderConversationID = "thread-1"
			cfg.FastMode = domain.AgentFastDefault
			replacement := makeService()
			t.Cleanup(func() { _ = replacement.Stop(ctx, testSession) })
			if _, err = replacement.Start(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			if resumed.FastMode != mode {
				t.Fatalf("restore fast=%q want pinned=%q", resumed.FastMode, mode)
			}
		})
	}
}
