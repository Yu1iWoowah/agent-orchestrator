package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	browsersvc "github.com/aoagents/agent-orchestrator/backend/internal/service/browser"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type prMovedAgents struct{}

func (prMovedAgents) Agent(domain.AgentHarness) (ports.Agent, bool) { return nil, false }

type prMovedLauncher struct {
	chatLauncher
	starts atomic.Int32
}

func (l *prMovedLauncher) StartChat(_ context.Context, config sessionmanager.ChatStart) (sessionmanager.ChatStarted, error) {
	l.starts.Add(1)
	started := sessionmanager.ChatStarted{
		ProviderConversationID: config.ProviderConversationID,
		ControllerGeneration:   "restarted",
	}
	_, err := config.ControllerReady(started)
	return started, err
}

type prMovedSCM struct{}

func (prMovedSCM) ParseRepository(string) (ports.SCMRepo, bool) { return ports.SCMRepo{}, false }

func (prMovedSCM) FetchPullRequests(context.Context, []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	return []ports.SCMObservation{{
		Fetched: true, Provider: "github", Host: "github.com", Repo: "example/project",
		PR: ports.SCMPRObservation{URL: "https://github.com/example/project/pull/1", Number: 1},
	}}, nil
}

func (prMovedSCM) FetchReviewThreads(context.Context, ports.SCMPRRef) (ports.SCMReviewObservation, error) {
	return ports.SCMReviewObservation{}, nil
}

type prMovedMessenger struct {
	manager *sessionmanager.Manager
	sent    chan error
}

func (m *prMovedMessenger) Send(ctx context.Context, id domain.SessionID, message string) error {
	err := m.manager.Send(ctx, id, message, nil)
	m.sent <- err
	return err
}

func TestPRMovedNoticeRespectsExplicitChatExit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		explicitExit bool
		wantStarts   int32
	}{
		{name: "explicit exit", explicitExit: true},
		{name: "ordinary stop", wantStarts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := sqlitetest.MustOpenAt(t, dir)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			now := time.Now().UTC()
			if err := st.UpsertProject(ctx, domain.ProjectRecord{
				ID: "p", Kind: domain.ProjectKindSingleRepo, Path: dir,
				RepoOriginURL: "https://github.com/example/project", RegisteredAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			newWorker := func(generation string, state domain.ActivityState) domain.SessionRecord {
				t.Helper()
				rec, err := st.CreateSession(ctx, domain.SessionRecord{
					ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat,
					Activity: domain.Activity{State: state, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now,
					Metadata: domain.SessionMetadata{WorkspacePath: dir, Branch: generation, ControllerGeneration: generation, ProviderConversationID: generation},
				})
				if err != nil {
					t.Fatal(err)
				}
				return rec
			}
			sourceState := domain.ActivityExited
			if tc.explicitExit {
				sourceState = domain.ActivityIdle
			}
			source, target := newWorker("source", sourceState), newWorker("target", domain.ActivityIdle)
			chatService := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, DataDir: dir})
			launcher := &prMovedLauncher{chatLauncher: chatLauncher{svc: chatService}}
			manager := sessionmanager.New(sessionmanager.Deps{
				Store: st, Lifecycle: lifecycle.New(st, nil), Chat: launcher, Agents: prMovedAgents{},
				BrowserCapabilities: browsersvc.NewAuthority(), DataDir: dir, BackgroundContext: ctx,
			})
			t.Cleanup(func() {
				cancel()
				_ = manager.WaitBackgroundWorkers(context.Background())
			})
			svc := sessionsvc.NewWithDeps(sessionsvc.Deps{
				Manager: manager, Store: st, PRClaimer: st, SCM: prMovedSCM{}, BackgroundContext: ctx,
			})
			messenger := &prMovedMessenger{manager: manager, sent: make(chan error, 2)}
			wireSessionFork(st, svc, nil, chatService, nil, nil, nil, messenger, slog.New(slog.DiscardHandler))
			if _, err := svc.ClaimPR(ctx, source.ID, "1", sessionsvc.ClaimPROptions{}); err != nil {
				t.Fatalf("seed PR owner: %v", err)
			}
			if tc.explicitExit {
				exited, err := svc.ExitAgent(ctx, source.ID)
				if err != nil || !exited.Session.ChatExplicitlyExited() {
					t.Fatalf("explicit Exit did not persist: %v", err)
				}
			}
			outcome, err := svc.ClaimPR(ctx, target.ID, "1", sessionsvc.ClaimPROptions{AllowTakeover: true})
			if err != nil || len(outcome.TakenOverFrom) != 1 || outcome.TakenOverFrom[0] != source.ID {
				t.Fatalf("PR takeover = %+v, %v", outcome, err)
			}
			// The second send follows resume admission. The background worker
			// is registered by then, even if it has already settled.
			for attempt := range 2 {
				select {
				case err := <-messenger.sent:
					if attempt == 0 && !errors.Is(err, chatsvc.ErrNoController) {
						t.Fatalf("first notice send = %v, want no controller", err)
					}
				case <-ctx.Done():
					t.Fatal("PR moved notice did not reach its send attempts")
				}
			}
			if err := manager.WaitBackgroundWorkers(ctx); err != nil {
				t.Fatal(err)
			}
			status, err := svc.ResumeAgentStatus(ctx, source.ID)
			if err != nil || launcher.starts.Load() != tc.wantStarts {
				t.Fatalf("PR takeover starts = %d, want %d, resume state = %s, error = %v", launcher.starts.Load(), tc.wantStarts, status.State, err)
			}
			if tc.explicitExit {
				var refusal *apierr.Error
				if status.State != sessionsvc.ResumeAgentFailed || !errors.As(status.Err, &refusal) || refusal.Code != "CHAT_EXPLICITLY_EXITED" ||
					!status.Session.ChatExplicitlyExited() || status.Session.Activity.State != domain.ActivityExited {
					t.Fatalf("explicit exit after PR takeover: state = %s, failure = %v, marker = %t, activity = %s",
						status.State, status.Err, status.Session.ChatExplicitlyExited(), status.Session.Activity.State)
				}
			} else if status.State != sessionsvc.ResumeAgentSucceeded {
				t.Fatalf("ordinary stop after PR takeover: state = %s, failure = %v", status.State, status.Err)
			}
		})
	}
}
