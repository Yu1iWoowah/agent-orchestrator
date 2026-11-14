package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	browsersvc "github.com/aoagents/agent-orchestrator/backend/internal/service/browser"
)

type resumeOutcome struct {
	result RestoreResult
	err    error
}

func startResume(t *testing.T, m *Manager, id domain.SessionID) (<-chan resumeOutcome, error) {
	t.Helper()
	done := make(chan resumeOutcome, 1)
	_, err := m.StartResumeAgent(ctx, id, func(result RestoreResult, err error) {
		done <- resumeOutcome{result: result, err: err}
	})
	return done, err
}

func TestStartResumeAgent_ReturnsBeforeTheLaunchAndReportsItsOutcome(t *testing.T) {
	baseRuntime := &fakeRuntime{aliveByHandle: map[string]bool{"tmux-mer-1": true}}
	runtime := &blockingRestartRuntime{fakeRuntime: baseRuntime, entered: make(chan struct{}), release: make(chan struct{})}
	agent := supervisedLaunchAgent{launchArgvAgent{argv: []string{"codex", "resume", "agent-x"}}}
	m, _, _ := newExitedResumeManager(t, runtime, agent)

	done, err := startResume(t, m, "mer-1")
	if err != nil {
		t.Fatalf("start resume: %v", err)
	}
	<-runtime.entered
	// The launch is still held, so the request returned before it.
	select {
	case got := <-done:
		t.Fatalf("resume settled while its launch was held: %+v", got)
	default:
	}
	if _, err := startResume(t, m, "mer-1"); !errors.Is(err, ErrResumeInProgress) {
		t.Fatalf("second start while resuming = %v, want ErrResumeInProgress", err)
	}
	close(runtime.release)
	got := <-done
	if got.err != nil || got.result.Mode != RestoreModeNative {
		t.Fatalf("background resume = %+v, want native success", got)
	}
}

func TestStartResumeAgent_WaitsForPersistentHostsInTheBackground(t *testing.T) {
	runtime := &fakeRuntime{aliveByHandle: map[string]bool{"tmux-mer-1": true}}
	agent := supervisedLaunchAgent{launchArgvAgent{argv: []string{"codex", "resume", "agent-x"}}}
	m, st, _ := newExitedResumeManager(t, runtime, agent)
	rec := st.sessions["mer-1"]
	slept := time.Unix(1, 0)
	rec.HibernatedAt = &slept
	st.sessions["mer-1"] = rec
	reconciled := make(chan struct{})
	m.persistentHostReconcileDone = reconciled

	started := time.Now()
	done, err := startResume(t, m, "mer-1")
	if err != nil {
		t.Fatalf("start resume: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("start waited %s for persistent hosts", elapsed)
	}
	select {
	case got := <-done:
		t.Fatalf("resume settled before persistent hosts reconciled: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(reconciled)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not continue after persistent hosts reconciled")
	}
}

func TestStartResumeAgent_AnswersRefusalsWithoutBackgroundWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*domain.SessionRecord)
		want error
	}{
		{"running agent", func(rec *domain.SessionRecord) { rec.Activity.State = domain.ActivityIdle }, ErrAgentNotExited},
		{"terminated", func(rec *domain.SessionRecord) { rec.IsTerminated = true }, ErrTerminated},
		{"provisioning", func(rec *domain.SessionRecord) { rec.ProvisionState = domain.SessionProvisionProvisioning }, ErrResumeInProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &fakeRuntime{aliveByHandle: map[string]bool{"tmux-mer-1": true}}
			agent := supervisedLaunchAgent{launchArgvAgent{argv: []string{"codex", "resume", "agent-x"}}}
			m, st, _ := newExitedResumeManager(t, runtime, agent)
			rec := st.sessions["mer-1"]
			tc.edit(&rec)
			st.sessions["mer-1"] = rec
			done, err := startResume(t, m, "mer-1")
			if !errors.Is(err, tc.want) {
				t.Fatalf("start resume = %v, want %v", err, tc.want)
			}
			if err := m.WaitBackgroundWorkers(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				t.Fatalf("refused resume still ran: %+v", got)
			default:
			}
			if runtime.created != 0 || runtime.destroyed != 0 {
				t.Fatalf("refused resume touched runtime: created=%d destroyed=%d", runtime.created, runtime.destroyed)
			}
		})
	}
	m, _, _ := newExitedResumeManager(t, &fakeRuntime{}, supervisedLaunchAgent{launchArgvAgent{argv: []string{"codex"}}})
	if _, err := startResume(t, m, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session = %v, want ErrNotFound", err)
	}
}

func TestStartResumeAgent_ReportsTheRetriedChatStartsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retryErr  error
		wantStart bool
	}{
		{name: "retry fails", retryErr: errors.New("temporary git failure")},
		{name: "retry succeeds", wantStart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launcher := &recordingLauncher{}
			m, st, _ := newChatManager(launcher)
			m.dataDir = t.TempDir()
			m.browserCapabilities = browsersvc.NewAuthority()
			works := make(chan func(), 4)
			m.runBackground = func(work func()) { works <- work }
			ws := m.workspace.(*fakeWorkspace)
			ws.createErr = errors.New("temporary git failure")
			rec, _, _, err := m.Spawn(context.Background(), asyncChatSpawnConfig("do the thing"))
			if err != nil {
				t.Fatal(err)
			}
			(<-works)()
			if st.sessions[rec.ID].ProvisionState != domain.SessionProvisionFailed {
				t.Fatalf("first start = %q, want failed", st.sessions[rec.ID].ProvisionState)
			}

			done, err := startResume(t, m, rec.ID)
			if err != nil {
				t.Fatalf("start resume: %v", err)
			}
			go (<-works)()
			retry := <-works
			// Resume handed the start to a new background start; it has not settled.
			select {
			case got := <-done:
				t.Fatalf("resume settled before the retried start ran: %+v", got)
			case <-time.After(50 * time.Millisecond):
			}
			ws.createErr = tc.retryErr
			retry()
			got := <-done
			if tc.wantStart {
				if got.err != nil || got.result.Mode != RestoreModeSavedPrompt || got.result.Session.ProvisionState != domain.SessionProvisionReady {
					t.Fatalf("resume = %+v, want the retried start ready", got)
				}
				return
			}
			var relaunch *RelaunchError
			if !errors.As(got.err, &relaunch) || relaunch.Step != "start" || !strings.Contains(got.err.Error(), "temporary git failure") {
				t.Fatalf("resume error = %v, want the retried start's failure", got.err)
			}
		})
	}
}
