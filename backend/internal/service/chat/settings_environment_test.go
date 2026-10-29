package chat_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type grantReconnectingDriver struct{ fakeDriver }

func (d grantReconnectingDriver) Reconnect(ctx context.Context, cfg ports.ChatResumeConfig) (ports.ChatConversation, error) {
	return d.Resume(ctx, cfg)
}

func TestSettingsGrantSurvivesLiveReconnectButNotProviderRelaunch(t *testing.T) {
	for _, harness := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		t.Run(string(harness), func(t *testing.T) {
			st := openStore(t)
			now := time.Now()
			actor, _, err := st.GetSession(t.Context(), testSession)
			if err != nil {
				t.Fatal(err)
			}
			actor.Kind, actor.Harness = domain.KindOrchestrator, harness
			if err := st.UpdateSession(t.Context(), actor); err != nil {
				t.Fatal(err)
			}
			target, err := st.CreateSession(t.Context(), domain.SessionRecord{ProjectID: testProject, Kind: domain.KindWorker, Harness: harness, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			signer, err := agentsettings.NewStoredGrantAuthority(t.Context(), st, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			service := func(driver ports.ChatDriver) *chatsvc.Service {
				s := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Reader: fullSnapshotReader(st), PageReader: boundedSnapshotReader(st), Activity: &recordingActivity{}, Drivers: fakeRegistry{driver: driver}, Log: slog.New(slog.DiscardHandler), Now: time.Now, NewID: uuid.NewString})
				s.SetAgentSettingsGrantEnvironment(agentsettings.NewGrantEnvironment(st, signer, time.Now))
				t.Cleanup(func() { s.StopAll(context.Background()) })
				return s
			}
			var launched ports.ChatStartConfig
			firstProvider := newFakeConversation()
			first := service(fakeDriver{conv: firstProvider, startCfg: &launched})
			initial, err := first.Start(t.Context(), chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: harness, WorkspacePath: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			grant := launched.Env[ports.EnvAgentSettingsGrant]
			if grant == "" {
				t.Fatal("native launch did not receive a settings grant")
			}
			first.StopAll(t.Context())
			actor, _, err = st.GetSession(t.Context(), testSession)
			if err != nil {
				t.Fatal(err)
			}
			actor.Metadata.ProviderConversationID = firstProvider.ProviderConversationID()
			if err := st.UpdateSession(t.Context(), actor); err != nil {
				t.Fatal(err)
			}
			live := &liveReconnectedConversation{nativeHistoryConversation: &nativeHistoryConversation{fakeConversation: newFakeConversation()}}
			second := service(grantReconnectingDriver{fakeDriver{conv: live}})
			reconnected, err := second.Start(t.Context(), chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: harness, ProviderConversationID: firstProvider.ProviderConversationID(), ReconnectOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if reconnected.Generation() == initial.Generation() {
				t.Fatal("live reconnect did not rotate the controller event/recovery fence")
			}
			authorizer := agentsettings.NewAuthorizer(st, signer, time.Now)
			if _, err := authorizer.Authorize(t.Context(), agentsettings.Caller{SessionID: testSession, Grant: grant}, target); err != nil {
				t.Fatalf("same native process lost its grant on live reconnect: %v", err)
			}
			second.StopAll(t.Context())
			var relaunched ports.ChatResumeConfig
			third := service(fakeDriver{conv: newFakeConversation(), resumeCfg: &relaunched})
			if _, err := third.Start(t.Context(), chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: harness, ProviderConversationID: firstProvider.ProviderConversationID()}); err != nil {
				t.Fatal(err)
			}
			if _, err := authorizer.Authorize(t.Context(), agentsettings.Caller{SessionID: testSession, Grant: grant}, target); err == nil {
				t.Fatal("replaced process retained its old grant")
			}
			if _, err := authorizer.Authorize(t.Context(), agentsettings.Caller{SessionID: testSession, Grant: relaunched.Env[ports.EnvAgentSettingsGrant]}, target); err != nil {
				t.Fatalf("fresh provider did not receive a usable replacement grant: %v", err)
			}
		})
	}
}

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
