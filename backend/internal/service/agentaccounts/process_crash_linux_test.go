package agentaccounts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type crashProcessProvider struct {
	*fakeClaude
	script string
}

func (p *crashProcessProvider) LoginArgv(context.Context) ([]string, error) {
	return []string{"/usr/bin/python3", p.script}, nil
}

type crashProcessRuntime struct {
	shellterm.ShellRuntime
	root  string
	crash bool
}

func (r *crashProcessRuntime) Create(_ context.Context, c ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.WorkspacePath
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e := cmd.Start(); e != nil {
		return ports.RuntimeHandle{}, e
	}
	if e := os.WriteFile(filepath.Join(r.root, "fake-runtime.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); e != nil {
		_ = cmd.Process.Kill()
		return ports.RuntimeHandle{}, e
	}
	if r.crash {
		os.Exit(77)
	}
	return ports.RuntimeHandle{ID: string(c.SessionID)}, nil
}
func (r *crashProcessRuntime) pid() (int, error) {
	b, e := os.ReadFile(filepath.Join(r.root, "fake-runtime.pid"))
	if e != nil {
		return 0, e
	}
	return strconv.Atoi(string(b))
}
func (r *crashProcessRuntime) Destroy(context.Context, ports.RuntimeHandle) error {
	pid, e := r.pid()
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	cmdline, e := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if len(cmdline) > 0 && !strings.Contains(string(cmdline), filepath.Join(r.root, "fake-login.py")) {
		return errors.New("refusing foreign test process")
	}
	if len(cmdline) > 0 {
		if e = syscall.Kill(pid, syscall.SIGKILL); e != nil && !errors.Is(e, syscall.ESRCH) {
			return e
		}
	}
	return nil
}
func (r *crashProcessRuntime) IsAlive(context.Context, ports.RuntimeHandle) (bool, error) {
	pid, e := r.pid()
	if errors.Is(e, os.ErrNotExist) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(e, os.ErrNotExist) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	tail := string(b)[strings.LastIndex(string(b), ")")+1:]
	return !strings.HasPrefix(strings.TrimSpace(tail), "Z"), nil
}
func (r *crashProcessRuntime) IsChildAlive(c context.Context, h ports.RuntimeHandle) (bool, error) {
	return r.IsAlive(c, h)
}

func TestLoginCrashChildProcess(t *testing.T) {
	root := os.Getenv("B2_FAKE_CRASH_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	db, e := sqlite.Open(filepath.Join(root, "db"))
	if e != nil {
		t.Fatal(e)
	}
	rt := &crashProcessRuntime{root: root, crash: true}
	term := shellterm.NewService(rt, db, nil, nil, root, "crashed-run", nil)
	p := &crashProcessProvider{fakeClaude: &fakeClaude{}, script: filepath.Join(root, "fake-login.py")}
	svc := New(db, []ports.AgentAccountProviderCLI{p}, filepath.Join(root, "accounts"), term, nil)
	_, e = svc.StartLogin(context.Background(), domain.AgentAccountProviderClaude)
	t.Fatalf("expected hard exit during runtime.Create: %v", e)
}
func TestHardDaemonExitDuringLoginLeavesRecoverablePTY(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "fake-login.py")
	if e := os.WriteFile(script, []byte("import time\nprint('fake-device-code-for-private-terminal',flush=True)\ntime.sleep(60)\n"), 0600); e != nil {
		t.Fatal(e)
	}
	rt := &crashProcessRuntime{root: root}
	t.Cleanup(func() { _ = rt.Destroy(context.Background(), ports.RuntimeHandle{}) })
	bin, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-test.run=^TestLoginCrashChildProcess$")
	cmd.Env = append(os.Environ(), "B2_FAKE_CRASH_ROOT="+root)
	output, e := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(e, &exit) || exit.ExitCode() != 77 {
		t.Fatalf("helper did not crash at runtime.Create: %v %s", e, output)
	}
	if alive, e := rt.IsAlive(ctx, ports.RuntimeHandle{}); e != nil || !alive {
		t.Fatal("fixture did not leave live orphaned fake login")
	}
	db, e := sqlite.Open(filepath.Join(root, "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = db.Close() }()
	ops, e := db.ListAgentAccountOperations(ctx)
	if e != nil || len(ops) != 1 || ops[0].Phase != "pending" {
		t.Fatal("hard exit lost login journal")
	}
	terms := shellterm.NewService(rt, db, nil, nil, root, "recovered-run", nil)
	p := &crashProcessProvider{fakeClaude: &fakeClaude{}, script: script}
	svc := New(db, []ports.AgentAccountProviderCLI{p}, filepath.Join(root, "accounts"), terms, nil)
	if e = svc.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	if e = svc.Recover(ctx); e != nil {
		t.Fatal(e)
	}
	for range 100 {
		alive, e := rt.IsAlive(ctx, ports.RuntimeHandle{})
		if e != nil {
			t.Fatal(e)
		}
		if !alive {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if alive, _ := rt.IsAlive(ctx, ports.RuntimeHandle{}); alive {
		t.Fatal("recovery left fake login process alive")
	}
	ops, _ = db.ListAgentAccountOperations(ctx)
	if ops[0].Phase != "complete" || ops[0].Outcome != "cancelled" {
		t.Fatal("recovery did not complete durable cancellation")
	}
	if _, e = os.Stat(filepath.Join(root, "accounts", "claude", ops[0].AccountID)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("recovery left pending profile")
	}
	data, e := os.ReadFile(filepath.Join(root, "db", "ao.db"))
	if e != nil || strings.Contains(string(data), "fake-device-code-for-private-terminal") {
		t.Fatal("login transcript reached durable DB")
	}
}
