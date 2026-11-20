package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type quitUsageStore struct {
	*sqlite.Store
	reads   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (s *quitUsageStore) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	// The collector's second read happens with its lock held.
	if s.reads.Add(1) == 2 {
		close(s.entered)
		<-s.release
	}
	return s.Store.GetSession(ctx, id)
}

type quitUsageRuntime struct{ ports.Runtime }

func (quitUsageRuntime) Destroy(context.Context, ports.RuntimeHandle) error { return nil }
func (quitUsageRuntime) ProbeFencedRuntime(context.Context, ports.FencedRuntimeRef) ports.FencedProbeResult {
	return ports.FencedProbeResult{Liveness: ports.FencedDead}
}

func TestDesktopQuitDrainsUsageBeforeExitAgent(t *testing.T) {
	st, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "usage-quit", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "usage-quit", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeTUI,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now,
		Metadata: domain.SessionMetadata{RuntimeHandleID: "fixture-runtime", RuntimeLaunchID: "fixture-launch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "project", "native-quit", "subagents", "agent-sub-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}

	blocked := &quitUsageStore{Store: st, entered: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(blocked.release) })
	collector := usagesvc.NewCollector(blocked, usagesvc.SourceRoots{ClaudeProjects: root}, nil)
	queue := usagesvc.NewHookQueue(collector, nil)
	workerCtx, cancelWorker := context.WithCancel(ctx)
	go queue.Run(workerCtx)
	defer func() {
		cancelWorker()
		release()
		_ = queue.CloseAndWait()
	}()
	if err := queue.RecordHook(ctx, rec.ID, usagesvc.HookSignal{Event: "session-start"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal("collector did not reach its locked session read")
	}
	if err := queue.RecordHook(ctx, rec.ID, usagesvc.HookSignal{
		Event: "subagent-stop", LaunchID: "fixture-launch", NativeSessionID: "native-quit",
		ProviderHint: "anthropic", ModelID: "claude-sonnet-4-6", SubagentID: "sub-1", SubagentTranscriptPath: path,
	}); err != nil {
		t.Fatal(err)
	}

	lcm := lifecycle.New(st, nil)
	lcm.SetUsageFinalizer(queue)
	manager := sessionmanager.New(sessionmanager.Deps{Store: st, Lifecycle: lcm, Runtime: quitUsageRuntime{}})
	cancelWorker()
	draining := make(chan struct{})
	stopping := make(chan struct{})
	result := make(chan error, 1)
	quitDone := make(chan struct{})
	go func() {
		defer close(quitDone)
		result <- stopDesktopControllers(5*time.Second, func() error {
			close(draining)
			return queue.CloseAndWait()
		}, func(ctx context.Context) error {
			close(stopping)
			return manager.StopAgentsForQuit(ctx)
		}, func(context.Context) error { return nil })
	}()
	defer func() {
		release()
		<-quitDone
	}()
	select {
	case <-stopping:
		t.Fatal("agent stopped before the usage drain")
	case <-draining:
	case <-ctx.Done():
		t.Fatal("desktop quit did not reach the usage drain")
	}
	current, found, err := st.GetSession(ctx, rec.ID)
	if err != nil || !found || current.Activity.State != domain.ActivityIdle {
		t.Fatalf("session while usage is blocked: state=%s found=%v err=%v", current.Activity.State, found, err)
	}
	release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("desktop quit did not complete after usage drain")
	}
	current, found, err = st.GetSession(ctx, rec.ID)
	if err != nil || !found || current.IsTerminated || current.Activity.State != domain.ActivityExited {
		t.Fatalf("session after quit: state=%s terminated=%v found=%v err=%v", current.Activity.State, current.IsTerminated, found, err)
	}
	binding, found, err := st.GetUsageBinding(ctx, rec.ID, rec.Harness, "native-quit")
	if err != nil || !found || binding.InitialModelID != "claude-sonnet-4-6" || binding.ProviderHint != "anthropic" {
		t.Fatalf("binding after quit: %+v found=%v err=%v", binding, found, err)
	}
	sources, err := st.ListUsageSourcesForBinding(ctx, binding.ID)
	if err != nil || len(sources) != 1 || sources[0].SubagentID != "sub-1" || sources[0].ArtifactPath != resolvedPath {
		t.Fatalf("sources after quit: %+v err=%v", sources, err)
	}
}
