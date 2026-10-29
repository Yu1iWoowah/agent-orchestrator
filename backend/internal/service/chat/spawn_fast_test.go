package chat_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type claudeFastConfigConversation struct {
	*fakeConversation
	configMu sync.Mutex
	fast     bool
	effort   string
}

func (c *claudeFastConfigConversation) ListConfigOptions(context.Context) ([]ports.ChatConfigOption, error) {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	return []ports.ChatConfigOption{
		{ID: "fast", Type: ports.ChatConfigOptionBoolean, Current: ports.ChatConfigOptionValue{Boolean: new(c.fast)}},
		{ID: "effort", Category: "thought_level", Type: ports.ChatConfigOptionSelect, Current: ports.ChatConfigOptionValue{Select: c.effort}},
	}, nil
}

func (c *claudeFastConfigConversation) SetConfigOption(ctx context.Context, id string, value ports.ChatConfigOptionValue) ([]ports.ChatConfigOption, error) {
	c.configMu.Lock()
	switch id {
	case "fast":
		c.fast = *value.Boolean
	case "effort":
		c.effort = value.Select
	}
	c.configMu.Unlock()
	return c.ListConfigOptions(ctx)
}

func TestClaudeEffortChoiceDoesNotCreateFastPreference(t *testing.T) {
	for _, tc := range []struct {
		name     string
		saved    domain.AgentFastMode
		reported bool
	}{
		{"default/provider on", domain.AgentFastDefault, true},
		{"default/provider off", domain.AgentFastDefault, false},
		{"explicit off/provider on", domain.AgentFastOff, true},
		{"explicit on/provider off", domain.AgentFastOn, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			rec, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			conv := &claudeFastConfigConversation{fakeConversation: newFakeConversation(), fast: tc.reported}
			svc := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Reader: fullSnapshotReader(st), Drivers: fakeRegistry{driver: fakeDriver{conv: conv}}, Log: slog.New(slog.DiscardHandler), NewID: uuid.NewString})
			if _, err := svc.Start(ctx, chatsvc.StartConfig{SessionID: rec.ID, ProjectID: testProject, Harness: domain.HarnessClaudeCode, WorkspacePath: t.TempDir(), FastMode: tc.saved}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = svc.Stop(ctx, rec.ID) })
			if _, err := svc.SetConfigOption(ctx, rec.ID, "effort", ports.ChatConfigOptionValue{Select: "high"}); err != nil {
				t.Fatal(err)
			}
			stored, err := st.ConversationForSession(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.saved
			if want == domain.AgentFastDefault {
				want = ""
			}
			if stored.Settings.ReasoningEffort != "high" || stored.Settings.AgentFastMode != want || stored.Settings.FastMode != (tc.saved == domain.AgentFastOn) {
				t.Fatalf("effort choice changed fast intent: %+v want fast=%q", stored.Settings, want)
			}
		})
	}
}

func TestClaudeConfigFastChoiceOverridesSpawnOnNextTurnAndRestart(t *testing.T) {
	for _, spawn := range []domain.AgentFastMode{domain.AgentFastOn, domain.AgentFastOff} {
		t.Run(string(spawn), func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			rec, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			var resumed ports.ChatResumeConfig
			makeService := func(conv *claudeFastConfigConversation) *chatsvc.Service {
				conv.providerConversationID = "claude-thread"
				return chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Reader: fullSnapshotReader(st), Drivers: fakeRegistry{driver: fakeDriver{conv: conv, resumeCfg: &resumed}}, Log: slog.New(slog.DiscardHandler), NewID: uuid.NewString})
			}
			conv := &claudeFastConfigConversation{fakeConversation: newFakeConversation(), fast: spawn == domain.AgentFastOn}
			svc := makeService(conv)
			t.Cleanup(func() { _ = svc.Stop(ctx, rec.ID) })
			cfg := chatsvc.StartConfig{SessionID: rec.ID, ProjectID: testProject, Harness: domain.HarnessClaudeCode, WorkspacePath: t.TempDir(), FastMode: spawn}
			if _, err := svc.Start(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			enabled := spawn != domain.AgentFastOn
			want := domain.AgentFastOff
			if enabled {
				want = domain.AgentFastOn
			}
			if _, err := svc.SetConfigOption(ctx, rec.ID, "fast", ports.ChatConfigOptionValue{Boolean: &enabled}); err != nil {
				t.Fatal(err)
			}
			stored, err := st.ConversationForSession(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Settings.AgentFastMode != want || stored.Settings.FastMode != enabled {
				t.Errorf("config-option preference not persisted: %+v want=%s", stored.Settings, want)
			}
			sent := make(chan struct{}, 1)
			conv.mu.Lock()
			conv.onSend = func(string) { sent <- struct{}{} }
			conv.mu.Unlock()
			if _, err := svc.Send(ctx, rec.ID, ports.ChatUserMessage{Text: "next turn"}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-sent:
			case <-time.After(5 * time.Second):
				t.Fatal("next turn never reached provider")
			}
			messages := conv.sentMessages()
			if len(messages) != 1 || messages[0].Settings.FastMode != want || messages[0].Settings.Fast != enabled {
				t.Errorf("next turn reapplied spawn fast: %+v want=%s", messages, want)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1"}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1", TurnState: domain.TurnStateCompleted})
			awaitStoreSnapshot(t, st, stored.ID, func(s store.ConversationSnapshot) bool {
				return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateCompleted
			})
			if err := svc.Stop(ctx, rec.ID); err != nil {
				t.Fatal(err)
			}
			replacement := makeService(&claudeFastConfigConversation{fakeConversation: newFakeConversation(), fast: enabled})
			cfg.ProviderConversationID = "claude-thread"
			if _, err := replacement.Start(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = replacement.Stop(ctx, rec.ID) })
			if resumed.FastMode != want {
				t.Errorf("restore reapplied spawn=%s: resumed=%s want=%s", spawn, resumed.FastMode, want)
			}
		})
	}
}

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
