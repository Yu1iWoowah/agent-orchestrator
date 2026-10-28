package chat_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

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
