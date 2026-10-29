package chat_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestSettingsGrantIsRedactedFromChatLaunchErrors(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "resume"}[resume], func(t *testing.T) {
			h, _, _, _ := journalHarness(t)
			if err := h.svc.Stop(t.Context(), testSession); err != nil {
				t.Fatal(err)
			}
			var token string
			fail := func(ctx context.Context, env map[string]string, prepare func(context.Context) (map[string]string, error)) (ports.ChatConversation, error) {
				if prepare != nil {
					var err error
					env, err = prepare(ctx)
					if err != nil {
						return nil, err
					}
				}
				token = env[ports.EnvAgentSettingsGrant]
				return nil, errors.New("fake native failure " + token)
			}
			driver := fakeDriver{conv: newFakeConversation(), start: func(cfg ports.ChatStartConfig) (ports.ChatConversation, error) {
				return fail(t.Context(), cfg.Env, cfg.PrepareEnv)
			}, resume: func(cfg ports.ChatResumeConfig) (ports.ChatConversation, error) {
				return fail(t.Context(), cfg.Env, cfg.PrepareEnv)
			}}
			svc := chatsvc.New(chatsvc.Options{Store: h.st, Sessions: h.st, Reader: fullSnapshotReader(h.st), PageReader: boundedSnapshotReader(h.st), Drivers: fakeRegistry{driver: driver}, Activity: h.activity, Log: slog.New(slog.DiscardHandler), Now: h.now, NewID: func() string { return "redaction-generation" }})
			signer, err := agentsettings.NewGrantAuthority([]byte(strings.Repeat("r", 32)), h.now)
			if err != nil {
				t.Fatal(err)
			}
			svc.SetAgentSettingsGrantEnvironment(agentsettings.NewGrantEnvironment(h.st, signer, h.now))
			cfg := chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(), PrepareControllerEnv: func(context.Context, domain.SessionControllerOwner) (map[string]string, error) {
				h.advance(time.Second)
				return map[string]string{ports.EnvAgentSettingsGrant: "untrusted-inherited"}, nil
			}}
			if resume {
				cfg.ProviderConversationID = "thread-1"
			}
			_, err = svc.Start(t.Context(), cfg)
			if err == nil || token == "" {
				t.Fatal("fake launch did not receive a scoped grant")
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), token) {
					t.Fatal("native grant escaped through Chat launch error chain")
				}
			}
		})
	}
}
