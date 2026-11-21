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
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func newRCAChatStore(t *testing.T, project domain.ProjectID, kind domain.SessionKind, harness domain.AgentHarness) (*sqlite.Store, domain.SessionID) {
	t.Helper()
	dir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dir)
	now := time.Now().UTC()
	if err := st.UpsertProject(t.Context(), domain.ProjectRecord{ID: string(project), Path: dir, RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateSession(t.Context(), domain.SessionRecord{ProjectID: project, Kind: kind, Harness: harness, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	record, ok, err := st.GetSession(t.Context(), created.ID)
	if err != nil || !ok || record.ProjectID != project || record.Kind != kind || record.Harness != harness {
		t.Fatal("inconsistent synthetic owner fixture")
	}
	return st, created.ID
}

type rcaClaudeObserver struct {
	prepare, ready, invalid, stopped atomic.Int64
	onReady                          func(ports.RCAClaudeLaunch)
	panicPrepare                     bool
}

func (o *rcaClaudeObserver) InvalidateClaudeLaunch(domain.SessionID)          { o.invalid.Add(1) }
func (o *rcaClaudeObserver) ClaudeControllerStopped(domain.SessionID, string) { o.stopped.Add(1) }
func (o *rcaClaudeObserver) PrepareClaudeEnvironment(_ context.Context, n ports.RCAClaudeLaunch, _ ports.RCAClaudeRuntime, _ []string) map[string]string {
	o.prepare.Add(1)
	if o.panicPrepare {
		panic("A_SYNTHETIC B_SYNTHETIC")
	}
	return map[string]string{"OTEL_LOGS_EXPORTER": "otlp"}
}
func (o *rcaClaudeObserver) ClaudeLaunchReady(_ context.Context, n ports.RCAClaudeLaunch, live bool) {
	o.ready.Add(1)
	if o.onReady != nil {
		o.onReady(n)
	}
}
func TestRCAClaudeMainLaunchCallbackAndPublication(t *testing.T) {
	for _, kind := range []domain.SessionKind{domain.KindWorker, domain.KindOrchestrator} {
		t.Run(string(kind), func(t *testing.T) {
			st, sessionID := newRCAChatStore(t, "agent-orchestrator", kind, domain.HarnessClaudeCode)
			observer := &rcaClaudeObserver{}
			driver := fakeDriver{conv: newFakeConversation()}
			driver.start = func(cfg ports.ChatStartConfig) (ports.ChatConversation, error) {
				if cfg.RCAEnvironment == nil {
					t.Fatal("main Claude launch callback absent")
				}
				delta := cfg.RCAEnvironment(t.Context(), ports.RCAClaudeRuntime{}, []string{"SYNTHETIC_NATIVE_KEY"})
				if delta["OTEL_LOGS_EXPORTER"] != "otlp" {
					t.Fatal("fake callback not forwarded")
				}
				return driver.conv, nil
			}
			svc := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Reader: fullSnapshotReader(st), Drivers: fakeRegistry{driver: driver}, Log: slog.New(slog.DiscardHandler), Now: time.Now, NewID: uuid.NewString})
			svc.SetRCAClaudeLaunchControl(observer)
			t.Cleanup(func() { svc.StopAll(context.Background()) })
			observer.onReady = func(n ports.RCAClaudeLaunch) {
				controller, err := svc.Controller(sessionID)
				if err != nil || controller.Generation() != n.Generation {
					t.Error("RCA activated before controller publication")
				}
			}
			cfg := chatsvc.StartConfig{SessionID: sessionID, ProjectID: "agent-orchestrator", Kind: kind, Harness: domain.HarnessClaudeCode, DataDir: t.TempDir(), WorkspacePath: t.TempDir()}
			if _, err := svc.Start(t.Context(), cfg); err != nil {
				t.Fatal(err)
			}
			if observer.prepare.Load() != 1 || observer.ready.Load() != 1 || observer.invalid.Load() != 1 {
				t.Fatal("incorrect main launch notifications")
			}
			if err := svc.Stop(t.Context(), sessionID); err != nil {
				t.Fatal(err)
			}
			if observer.stopped.Load() == 0 {
				t.Fatal("controller stop did not withdraw RCA")
			}
		})
	}
}
func TestRCAClaudeExcludedConsumersAndOptionalFailure(t *testing.T) {
	for _, scenario := range []string{"other-project", "codex", "review", "optional-panic"} {
		t.Run(scenario, func(t *testing.T) {
			project := domain.ProjectID("agent-orchestrator")
			harness := domain.HarnessClaudeCode
			if scenario == "other-project" {
				project = testProject
			}
			if scenario == "codex" {
				harness = domain.HarnessCodex
			}
			st, sessionID := newRCAChatStore(t, project, domain.KindWorker, harness)
			observer := &rcaClaudeObserver{panicPrepare: scenario == "optional-panic"}
			driver := fakeDriver{conv: newFakeConversation()}
			driver.start = func(cfg ports.ChatStartConfig) (ports.ChatConversation, error) {
				if scenario == "optional-panic" {
					if cfg.RCAEnvironment == nil {
						t.Fatal("missing callback")
					}
					if cfg.RCAEnvironment(t.Context(), ports.RCAClaudeRuntime{}, nil) != nil {
						t.Fatal("optional panic was not masked")
					}
				} else if cfg.RCAEnvironment != nil {
					t.Fatal("excluded consumer received RCA callback")
				}
				return driver.conv, nil
			}
			svc := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Drivers: fakeRegistry{driver: driver}, Log: slog.New(slog.DiscardHandler), NewID: uuid.NewString})
			svc.SetRCAClaudeLaunchControl(observer)
			t.Cleanup(func() { svc.StopAll(context.Background()) })
			cfg := chatsvc.StartConfig{SessionID: sessionID, ProjectID: project, Kind: domain.KindWorker, Harness: harness}
			if scenario == "review" {
				now := time.Now()
				if err := st.UpsertReview(t.Context(), domain.Review{ID: "rca-review", SessionID: sessionID, ProjectID: cfg.ProjectID, Harness: domain.ReviewerHarness(cfg.Harness), CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				cfg.Owner = domain.ReviewConversationOwner("rca-review")
			}
			if _, err := svc.Start(t.Context(), cfg); err != nil {
				t.Fatal("optional RCA affected native success:", err)
			}
			if scenario != "optional-panic" && (observer.prepare.Load() != 0 || observer.ready.Load() != 0) {
				t.Fatal("excluded consumer acquired capture")
			}
		})
	}
}
