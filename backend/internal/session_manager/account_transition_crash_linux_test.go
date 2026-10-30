package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// A separate fake provider holds a kernel lock after its daemon parent exits.
// Recovery must account for that writer, including the Create/receipt gap.
func TestAccountTransitionHardExitWriterMatrix(t *testing.T) {
	for _, stage := range []string{"preflight", "source-stopped", "intent", "create-gap", "receipt", "binding", "rollback"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			runtime := &crashWriterRuntime{fakeRuntime: &fakeRuntime{}, root: root}
			t.Cleanup(func() {
				for _, id := range []string{"source", "target", "rollback"} {
					_ = runtime.Destroy(context.Background(), ports.RuntimeHandle{ID: id})
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAccountTransitionCrashHelper$")
			cmd.Env = append(os.Environ(), "U3_CRASH_ROOT="+root, "U3_CRASH_STAGE="+stage)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("helper did not reach hard exit: %v %s", err, out)
			}
			db, err := sqlite.OpenPreMigrated(filepath.Join(root, "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sessions, err := db.ListAllSessions(ctx)
			if err != nil || len(sessions) != 1 {
				t.Fatal("missing durable session", err)
			}
			rec := sessions[0]
			manager, _, _, _, accounts, _ := newAccountTransitionManager(t, domain.SessionModeTUI)
			manager.store = db
			manager.runtime = runtime
			bound, _, err := db.GetSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderClaude)
			if err != nil {
				t.Fatal(err)
			}
			accounts.bound = map[domain.SessionID]string{rec.ID: bound}
			before := runtime.writers(t)
			if before > 1 {
				t.Fatalf("multiple writers survived daemon exit: %d", before)
			}
			if _, err := manager.recoverInterruptedInterfaceTransitions(ctx); err != nil {
				t.Fatal(err)
			}
			current, found, err := db.GetSessionInterfaceTransition(ctx, "move")
			if err != nil || !found {
				t.Fatal(err)
			}
			uncertain := stage == "intent" || stage == "create-gap"
			if uncertain {
				if !current.Active() || current.ErrorCode != "TARGET_STOP_UNCONFIRMED" || !manager.SessionMutationInProgress(rec.ID) {
					t.Fatalf("unknown writer not fenced: %+v", current)
				}
				if release, ok := manager.AcquireSessionInput(rec.ID); ok {
					release()
					t.Fatal("unknown writer accepts input")
				}
				if err := manager.ExitAgentsOnAccount(ctx, "B"); !errors.Is(err, ports.ErrAgentAccountInUse) {
					t.Fatal("unknown target profile can be removed", err)
				}
			} else if current.Active() {
				t.Fatalf("confirmed recovery stayed active: %+v", current)
			}
			want := 0
			if stage == "preflight" || stage == "binding" || stage == "create-gap" {
				want = 1
			}
			if got := runtime.writers(t); got != want {
				t.Fatalf("writer count=%d want=%d, transition=%+v", got, want, current)
			}
			actual, _, err := db.GetSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderClaude)
			if err != nil || actual != bound {
				t.Fatal("recovery silently changed binding", actual, err)
			}
		})
	}
}

func TestAccountTransitionCrashHelper(t *testing.T) {
	root, stage := os.Getenv("U3_CRASH_ROOT"), os.Getenv("U3_CRASH_STAGE")
	if root == "" {
		t.Skip("subprocess helper")
	}
	ctx := context.Background()
	db := sqlitetest.MustOpenAt(t, filepath.Join(root, "db"))
	now := time.Now().UTC()
	if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "fake", Path: root, RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	rec, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "fake", Harness: domain.HarnessClaudeCode, Kind: domain.KindWorker, Mode: domain.SessionModeTUI, Activity: domain.Activity{State: domain.ActivityExited, LastActivityAt: now}, Metadata: domain.SessionMetadata{RuntimeHandleID: "source", RuntimeLaunchID: "source-generation", AgentSessionID: "same-native"}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderClaude, "A", now); err != nil {
		t.Fatal(err)
	}
	runtime := &crashWriterRuntime{fakeRuntime: &fakeRuntime{}, root: root}
	// All phases after preflight have positively stopped the source before any
	// target is permitted to launch, as enforced by the production drain suite.
	phase := domain.SessionInterfaceTransitionTargetStarting
	if stage == "preflight" {
		phase = domain.SessionInterfaceTransitionPreflighting
		runtime.start(t, "source")
	}
	if stage == "source-stopped" {
		phase = domain.SessionInterfaceTransitionSourceStopped
	}
	_, created, err := db.CreateSessionInterfaceTransition(ctx, domain.SessionInterfaceTransition{ID: "move", SessionID: rec.ID, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, Policy: domain.SessionInterfaceTransitionDrain, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict, Phase: phase, SourceAccountID: "A", TargetAccountID: "B", NativeConversationID: "same-native", CreatedAt: now, UpdatedAt: now})
	if err != nil || !created {
		t.Fatal(err)
	}
	if stage != "preflight" && stage != "source-stopped" {
		manager, _, _, _, _, _ := newAccountTransitionManager(t, domain.SessionModeTUI)
		manager.store = db
		manager.runtime = runtime
		launchCtx := context.WithValue(ctx, accountTransitionRuntimeContextKey{}, "move")
		if err := manager.recordAccountRuntime(launchCtx, domain.AccountTransitionRuntimeReserved, "target-generation", ""); err != nil {
			t.Fatal(err)
		}
		if stage != "intent" {
			runtime.start(t, "target")
			if stage != "create-gap" {
				if err := manager.accountRuntimeCreated(launchCtx, "target-generation", ports.RuntimeHandle{ID: "target"}, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
		if stage == "binding" {
			if err := db.BindSessionAgentAccountWithSource(ctx, rec.ID, domain.ResolvedAgentAccount{AccountID: "B", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceExplicit}, now); err != nil {
				t.Fatal(err)
			}
		}
		if stage == "rollback" {
			if err := manager.stopAccountTransitionRuntime(ctx, domain.SessionInterfaceTransition{ID: "move"}); err != nil {
				t.Fatal(err)
			}
			if err := manager.recordAccountRuntime(launchCtx, domain.AccountTransitionRuntimeReserved, "rollback-generation", ""); err != nil {
				t.Fatal(err)
			}
			runtime.start(t, "rollback")
			if err := manager.accountRuntimeCreated(launchCtx, "rollback-generation", ports.RuntimeHandle{ID: "rollback"}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	os.Exit(73) // deliberately bypass database close, defers and test cleanup
}

func TestAccountTransitionFakeWriter(t *testing.T) {
	path := os.Getenv("U3_WRITER_PATH")
	if path == "" {
		t.Skip("fake provider subprocess")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".ready", []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if _, err := os.Lstat(path + ".stop"); err == nil {
				return
			}
		case <-deadline.C:
			return
		}
	}
}

type crashWriterRuntime struct {
	*fakeRuntime
	root string
}

func (r *crashWriterRuntime) start(t *testing.T, id string) {
	t.Helper()
	path := filepath.Join(r.root, id)
	cmd := exec.Command(os.Args[0], "-test.run=^TestAccountTransitionFakeWriter$")
	cmd.Env = append(os.Environ(), "U3_WRITER_PATH="+path, "U3_CRASH_ROOT=")
	// The writer inherits neither an output pipe nor a daemon-owned context.
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Lstat(path + ".ready"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("fake provider did not signal readiness")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func (r *crashWriterRuntime) IsAlive(_ context.Context, h ports.RuntimeHandle) (bool, error) {
	f, err := os.OpenFile(filepath.Join(r.root, h.ID), os.O_RDWR, 0600)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false, nil
}
func (r *crashWriterRuntime) Destroy(ctx context.Context, h ports.RuntimeHandle) error {
	if err := os.WriteFile(filepath.Join(r.root, h.ID)+".stop", []byte("stop"), 0600); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		alive, err := r.IsAlive(ctx, h)
		if err != nil || !alive {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fake provider stop unconfirmed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func (r *crashWriterRuntime) writers(t *testing.T) int {
	t.Helper()
	n := 0
	for _, id := range []string{"source", "target", "rollback"} {
		alive, err := r.IsAlive(context.Background(), ports.RuntimeHandle{ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if alive {
			n++
		}
	}
	return n
}
