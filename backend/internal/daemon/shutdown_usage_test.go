package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type quitUsageStore struct {
	*sqlite.Store
	reads     atomic.Int32
	entered   chan struct{}
	release   chan struct{}
	recordCtx chan context.Context
}

func (s *quitUsageStore) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	// The collector's second read happens with its lock held.
	if s.reads.Add(1) == 2 {
		if s.recordCtx != nil {
			s.recordCtx <- ctx
		}
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

func seedShutdownUsageSession(ctx context.Context, t *testing.T, mode domain.SessionMode) (*sqlite.Store, domain.SessionRecord) {
	t.Helper()
	st, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UTC()
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "usage-quit", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	metadata := domain.SessionMetadata{}
	if mode == domain.SessionModeTUI {
		metadata.RuntimeHandleID = "fixture-runtime"
		metadata.RuntimeLaunchID = "fixture-launch"
	}
	rec, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "usage-quit", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: mode,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now,
		Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, rec
}

func TestDesktopQuitDrainsUsageBeforeExitAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	st, rec := seedShutdownUsageSession(ctx, t, domain.SessionModeTUI)
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
		_ = queue.CloseAndWait(ctx)
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
		result <- stopControllersAfterUsageDrain(5*time.Second, 4*time.Second, func(drainCtx context.Context) error {
			close(draining)
			return queue.CloseAndWait(drainCtx)
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

type shutdownChatDriver struct {
	ports.ChatDriver
	provider *shutdownChatProvider
}

func (d shutdownChatDriver) Harness() domain.AgentHarness { return domain.HarnessClaudeCode }
func (d shutdownChatDriver) Probe(context.Context) (ports.ChatCapabilities, error) {
	return d.provider.Capabilities(), nil
}
func (d shutdownChatDriver) Start(context.Context, ports.ChatStartConfig) (ports.ChatConversation, error) {
	return d.provider, nil
}

type shutdownChatRegistry struct{ driver shutdownChatDriver }

func (r shutdownChatRegistry) Driver(domain.AgentHarness) (ports.ChatDriver, error) {
	return r.driver, nil
}
func (shutdownChatRegistry) SupportsChat(domain.AgentHarness) bool { return true }

type shutdownChatProvider struct {
	ports.ChatConversation
	events     chan ports.ChatEvent
	once       sync.Once
	terminated bool
}

func (*shutdownChatProvider) ProviderConversationID() string { return "shutdown-native" }
func (*shutdownChatProvider) Capabilities() ports.ChatCapabilities {
	return ports.ChatCapabilities{
		ports.ChatCapabilityStreaming: true, ports.ChatCapabilityApprovals: true,
		ports.ChatCapabilityInterrupt: true, ports.ChatCapabilityResume: true,
	}
}
func (*shutdownChatProvider) PreservesProviderOnClose() bool   { return true }
func (p *shutdownChatProvider) Events() <-chan ports.ChatEvent { return p.events }
func (p *shutdownChatProvider) Close() error {
	p.once.Do(func() { close(p.events) })
	return nil
}
func (p *shutdownChatProvider) Terminate() error {
	p.terminated = true
	return p.Close()
}

func TestControllerShutdownKeepsStopBudgetWhenUsageRecorderStaysBlocked(t *testing.T) {
	t.Run("desktop quit", func(t *testing.T) { testControllerShutdownWithBlockedUsage(t, true) })
	t.Run("daemon replacement", func(t *testing.T) { testControllerShutdownWithBlockedUsage(t, false) })
}

func testControllerShutdownWithBlockedUsage(t *testing.T, desktopQuit bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	mode := domain.SessionModeTUI
	if !desktopQuit {
		mode = domain.SessionModeChat
	}
	st, rec := seedShutdownUsageSession(ctx, t, mode)
	var provider *shutdownChatProvider
	chat := chatsvc.New(chatsvc.Options{Store: st, Sessions: st})
	if !desktopQuit {
		provider = &shutdownChatProvider{events: make(chan ports.ChatEvent)}
		t.Cleanup(func() { _ = provider.Close() })
		chat = chatsvc.New(chatsvc.Options{Store: st, Sessions: st, NewID: uuid.NewString, Drivers: shutdownChatRegistry{
			driver: shutdownChatDriver{provider: provider},
		}})
		if _, err := chat.Start(ctx, chatsvc.StartConfig{
			SessionID: rec.ID, ProjectID: rec.ProjectID, Harness: rec.Harness,
			WorkspacePath: t.TempDir(), Kind: rec.Kind,
		}); err != nil {
			t.Fatal(err)
		}
	}
	blocked := &quitUsageStore{
		Store: st, entered: make(chan struct{}), release: make(chan struct{}), recordCtx: make(chan context.Context, 1),
	}
	var logs bytes.Buffer
	collector := usagesvc.NewCollector(blocked, usagesvc.SourceRoots{}, nil)
	queue := usagesvc.NewHookQueue(collector, slog.New(slog.NewJSONHandler(&logs, nil)))
	workerCtx, cancelWorker := context.WithCancel(ctx)
	go queue.Run(workerCtx)
	defer func() {
		cancelWorker()
		close(blocked.release)
		select {
		case <-queue.Done():
		case <-ctx.Done():
			t.Error("usage worker did not exit after fixture released")
		}
	}()
	if err := queue.RecordHook(ctx, rec.ID, usagesvc.HookSignal{Event: "session-start"}); err != nil {
		t.Fatal(err)
	}
	var recordCtx context.Context
	select {
	case recordCtx = <-blocked.recordCtx:
	case <-ctx.Done():
		t.Fatal("collector did not start")
	}
	if err := queue.RecordHook(ctx, rec.ID, usagesvc.HookSignal{Event: "subagent-stop"}); err != nil {
		t.Fatal(err)
	}
	cancelWorker()
	lcm := lifecycle.New(st, nil)
	lcm.SetUsageFinalizer(queue)
	manager := sessionmanager.New(sessionmanager.Deps{Store: st, Lifecycle: lcm, Runtime: quitUsageRuntime{}})
	var stopContextErrs []error
	checkContext := func(stop func(context.Context) error) func(context.Context) error {
		return func(ctx context.Context) error {
			stopContextErrs = append(stopContextErrs, ctx.Err())
			return stop(ctx)
		}
	}
	stops := []func(context.Context) error{checkContext(chat.StopAll)}
	if desktopQuit {
		stops = []func(context.Context) error{checkContext(manager.StopAgentsForQuit), checkContext(chat.TerminateAll)}
	}
	const budget = 2 * time.Second
	type shutdownResult struct {
		err     error
		elapsed time.Duration
	}
	result := make(chan shutdownResult, 1)
	go func() {
		started := time.Now()
		err := stopControllersAfterUsageDrain(budget, budget, queue.CloseAndWait, stops...)
		result <- shutdownResult{err: err, elapsed: time.Since(started)}
	}()
	select {
	case got := <-result:
		if !errors.Is(got.err, context.DeadlineExceeded) || got.elapsed >= budget {
			t.Fatalf("shutdown returned %v after %s, want usage deadline within %s", got.err, got.elapsed, budget)
		}
	case <-time.After(2 * budget):
		t.Fatal("usage recorder kept shutdown blocked")
	}
	if len(stopContextErrs) != len(stops) {
		t.Fatalf("stop calls = %d, want %d", len(stopContextErrs), len(stops))
	}
	for i, err := range stopContextErrs {
		if err != nil {
			t.Fatalf("stop %d received expired context: %v", i, err)
		}
	}
	select {
	case <-queue.Done():
		t.Fatal("fixture unexpectedly released the recorder")
	case <-recordCtx.Done():
	case <-ctx.Done():
		t.Fatal("drain timeout did not cancel collector context")
	}
	current, found, err := st.GetSession(ctx, rec.ID)
	if err != nil || !found {
		t.Fatalf("session after shutdown: found=%v err=%v", found, err)
	}
	if desktopQuit {
		if current.Activity.State != domain.ActivityExited {
			t.Fatalf("after desktop quit: state=%s, want exited", current.Activity.State)
		}
	} else {
		if chat.HasLiveChatController(rec.ID) || provider.terminated || current.Activity.State != domain.ActivityIdle {
			t.Fatalf("after replacement: live=%v terminated=%v state=%s, want detached resumable Chat",
				chat.HasLiveChatController(rec.ID), provider.terminated, current.Activity.State)
		}
	}
	var entry struct {
		Message  string                   `json:"msg"`
		Pending  int                      `json:"pending_events"`
		Sessions map[domain.SessionID]int `json:"pending_by_session"`
	}
	if err := json.NewDecoder(&logs).Decode(&entry); err != nil || entry.Message != "usage hook drain incomplete" || entry.Pending != 2 || entry.Sessions[rec.ID] != 2 {
		t.Fatalf("incomplete drain diagnostic = %+v, err=%v", entry, err)
	}
}
