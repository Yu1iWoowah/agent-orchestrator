//go:build !windows

package review

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/tmux"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type shellReviewer struct{ fakeReviewer }

type recordingCreateRuntime struct {
	*tmux.Runtime
	created fakeRuntime
}

func (r *recordingCreateRuntime) Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	return r.created.Create(ctx, cfg)
}

func (*shellReviewer) ReviewCommand(context.Context, ports.ReviewInvocation) (ports.ReviewCommandSpec, error) {
	return ports.ReviewCommandSpec{Argv: []string{"/bin/sh", "-c", "echo reviewer-ready; exec /bin/cat"}}, nil
}

func TestLauncherSpawnWithMissingOrLiveTmuxPane(t *testing.T) {
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	// Keep the socket path short and isolate both private and legacy discovery.
	socketDir, err := os.MkdirTemp("", "r-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Setenv("TMUX", "")
	const socket = "review-test"
	run := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := exec.Command(binary, append([]string{"-L", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() { _ = exec.Command(binary, "-L", socket, "kill-server").Run() })
	workspace := t.TempDir()
	// A longer prefix match must survive both missing-target cleanup and replacement.
	const decoy = "review-mer-1-other"
	run(t, "-f", os.DevNull, "new-session", "-d", "-s", decoy, "-c", workspace, "/bin/cat")
	decoyPane := run(t, "list-panes", "-s", "-t", "="+decoy+":", "-F", "#{pane_id}")
	rt := tmux.New(tmux.Options{Binary: binary, SocketName: socket, Shell: "/bin/sh"})
	l := newTestLauncher(t, &shellReviewer{}, rt)
	spec := launchSpec()
	spec.WorkspacePath = workspace
	for _, state := range []string{"missing", "live"} {
		t.Run(state, func(t *testing.T) {
			oldPane := ""
			if state == "live" {
				run(t, "new-session", "-d", "-s", "review-mer-1", "-c", workspace, "/bin/cat")
				oldPane = run(t, "list-panes", "-s", "-t", "=review-mer-1:", "-F", "#{pane_id}")
			}
			t.Cleanup(func() { _ = rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "review-mer-1"}) })
			launch, err := l.Spawn(context.Background(), spec)
			if err != nil {
				t.Fatalf("Spawn with %s pane: %v", state, err)
			}
			if launch.HandleID != "review-mer-1" {
				t.Fatalf("handle = %q, want review-mer-1", launch.HandleID)
			}
			pane := run(t, "list-panes", "-s", "-t", "=review-mer-1:", "-F", "#{pane_id}")
			if pane == "" || pane == oldPane {
				t.Fatal("Spawn reused the old pane instead of replacing it")
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				out, err := rt.GetOutput(context.Background(), ports.RuntimeHandle{ID: launch.HandleID}, 100)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(out, "reviewer-ready") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("reviewer command did not run: %s", out)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if got := run(t, "list-panes", "-s", "-t", "="+decoy+":", "-F", "#{pane_id}"); got != decoyPane {
				t.Fatalf("other reviewer's pane changed: got %q, want %q", got, decoyPane)
			}
		})
	}
	t.Run("completed_run_retrigger_without_claim", func(t *testing.T) {
		completed := domain.ReviewRun{
			ID: "run-complete", ReviewID: "rev-1", SessionID: "mer-1", Harness: domain.ReviewerClaudeCode,
			PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha0",
			Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
		}
		store := &fakeStore{
			review: &domain.Review{
				ID: "rev-1", SessionID: "mer-1", Harness: domain.ReviewerClaudeCode,
				ReviewerHandleID: "review-mer-1", AgentSessionID: "native-reviewer-1",
			},
			runs: []domain.ReviewRun{completed},
		}
		// Use real tmux cleanup, recording Create without starting a supervisor.
		runtime := &recordingCreateRuntime{Runtime: rt}
		launcher := newTestLauncher(t, &shellReviewer{}, runtime)
		engine := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, launcher)
		result, err := trigger(context.Background(), engine, "mer-1", "", domain.AgentConfig{})
		if err != nil {
			t.Fatalf("Trigger after completed review with missing pane: %v", err)
		}
		if !result.Created || result.Run.Status != domain.ReviewRunRunning || result.ReviewerHandleID != "review-mer-1" || !runtime.created.created {
			t.Fatalf("reviewer did not relaunch: %+v", result)
		}
		if store.runs[0] != completed {
			t.Fatalf("completed review changed during retrigger: %+v", store.runs[0])
		}
		if got := run(t, "list-panes", "-s", "-t", "="+decoy+":", "-F", "#{pane_id}"); got != decoyPane {
			t.Fatalf("other reviewer's pane changed: got %q, want %q", got, decoyPane)
		}
	})
	t.Run("missing_record_teardown", func(t *testing.T) {
		store := &fakeStore{review: &domain.Review{
			ID: "rev-1", SessionID: "mer-1", Harness: domain.ReviewerClaudeCode,
			ReviewerHandleID: "review-mer-1",
		}}
		engine := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), fakeProjects{}, l)
		if _, err := engine.TerminateReviewer(context.Background(), "mer-1", ""); err != nil {
			t.Fatalf("TerminateReviewer with missing pane: %v", err)
		}
		if store.review.ReviewerHandleID != "" {
			t.Fatalf("stale reviewer handle retained: %q", store.review.ReviewerHandleID)
		}
		if got := run(t, "list-panes", "-s", "-t", "="+decoy+":", "-F", "#{pane_id}"); got != decoyPane {
			t.Fatalf("other reviewer's pane changed: got %q, want %q", got, decoyPane)
		}
	})
}
