package chat_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type reportingObserverFunc func(ports.HCGReportingLaunch)

func (f reportingObserverFunc) ReportingLaunchReady(n ports.HCGReportingLaunch) { f(n) }

func TestReportingLaunchAfterChatPublication(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	var notices []ports.HCGReportingLaunch
	newService := func(driver ports.ChatDriver) *chatsvc.Service {
		s := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Reader: fullSnapshotReader(st), PageReader: boundedSnapshotReader(st), Activity: &recordingActivity{}, Drivers: fakeRegistry{driver: driver}, Log: slog.New(slog.DiscardHandler), Now: time.Now, NewID: uuid.NewString})
		s.SetHCGReportingObserver(reportingObserverFunc(func(n ports.HCGReportingLaunch) {
			c, err := s.Controller(testSession)
			if err != nil || c.Generation() != n.CommittedOwner.ControllerGeneration {
				t.Errorf("notification before publication: %+v %v", n, err)
			}
			rec, ok, err := st.GetSession(t.Context(), testSession)
			if err != nil || !ok || rec.ControllerOwner().ControllerGeneration != n.CommittedOwner.ControllerGeneration {
				t.Errorf("notification before durable owner: %+v %v", rec, err)
			}
			notices = append(notices, n)
		}))
		t.Cleanup(func() { s.StopAll(context.Background()) })
		return s
	}
	cfg := chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, DataDir: root, WorkspacePath: t.TempDir(), ArtifactDir: t.TempDir()}
	provider := newFakeConversation()
	first := newService(fakeDriver{conv: provider})
	if _, err := first.Start(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	first.StopAll(t.Context())
	live := &liveReconnectedConversation{nativeHistoryConversation: &nativeHistoryConversation{fakeConversation: newFakeConversation()}}
	second := newService(grantReconnectingDriver{fakeDriver{conv: live}})
	cfg.ProviderConversationID = provider.ProviderConversationID()
	cfg.ReconnectOnly = true
	if _, err := second.Start(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 2 || !notices[0].Fresh || notices[1].Fresh || notices[0].CommittedOwner.ControllerGeneration == notices[1].CommittedOwner.ControllerGeneration || notices[0].DataRoot != root || notices[0].ArtifactDir != cfg.ArtifactDir {
		t.Fatalf("fresh/live notification: %+v", notices)
	}
}

func TestReportingLaunchChatFailureAndReviewerExcluded(t *testing.T) {
	for _, stage := range []string{"failed", "ready_failed", "review"} {
		t.Run(stage, func(t *testing.T) {
			review := stage == "review"
			st := openStore(t)
			driver := fakeDriver{conv: newFakeConversation()}
			if stage == "failed" {
				driver.start = func(ports.ChatStartConfig) (ports.ChatConversation, error) { return nil, errors.New("native failure") }
			}
			s := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Drivers: fakeRegistry{driver: driver}, Log: slog.New(slog.DiscardHandler), NewID: uuid.NewString})
			t.Cleanup(func() { s.StopAll(context.Background()) })
			count := 0
			s.SetHCGReportingObserver(reportingObserverFunc(func(ports.HCGReportingLaunch) { count++ }))
			cfg := chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex}
			readyCalled := false
			if stage == "ready_failed" {
				cfg.ControllerReady = func(ports.ChatControllerStarted) (ports.ChatControllerCommit, error) {
					readyCalled = true
					return ports.ChatControllerCommit{}, errors.New("durable ready failure")
				}
			}

			if review {
				now := time.Now()
				if err := st.UpsertReview(t.Context(), domain.Review{ID: "review-1", SessionID: testSession, ProjectID: testProject, Harness: domain.ReviewerCodex, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
				cfg.Owner = domain.ReviewConversationOwner("review-1")
			}
			_, err := s.Start(t.Context(), cfg)
			if (err == nil) != review {
				t.Fatalf("Start error=%v review=%v", err, review)
			}
			if stage == "ready_failed" && !readyCalled {
				t.Fatal("did not reach ControllerReady failure")
			}
			if count != 0 {
				t.Fatalf("non-main success notification count=%d", count)
			}
		})
	}
}

func TestReportingLaunchPanicDoesNotUndoChatSuccess(t *testing.T) {
	st := openStore(t)
	s := chatsvc.New(chatsvc.Options{Store: st, Sessions: st, Drivers: fakeRegistry{driver: fakeDriver{conv: newFakeConversation()}}, Log: slog.New(slog.DiscardHandler), NewID: uuid.NewString})
	t.Cleanup(func() { s.StopAll(context.Background()) })
	s.SetHCGReportingObserver(reportingObserverFunc(func(ports.HCGReportingLaunch) { panic("optional reporting failure") }))
	c, err := s.Start(t.Context(), chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex})
	if err != nil {
		t.Fatal(err)
	}
	if current, err := s.Controller(testSession); err != nil || current != c {
		t.Fatalf("successful native controller lost: %v", err)
	}
}
