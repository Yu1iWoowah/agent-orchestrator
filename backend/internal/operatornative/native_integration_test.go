package operatornative

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/appdiagnostics"
	"github.com/aoagents/agent-orchestrator/backend/internal/browserruntime"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type nativeFixtureState struct {
	AuthorityGo      bool                       `json:"authorityGo"`
	IssuerEpoch      string                     `json:"issuerEpoch"`
	ActionState      domain.OperatorActionState `json:"actionState"`
	MaterialSafe     bool                       `json:"materialSafe"`
	ChallengeScanned bool                       `json:"challengeScanned"`
}

// The real native test is opt-in. Ordinary tests do not launch Electron, use
// synthetic gestures or imply that a fake dialog passed physical validation.
func TestC0NativeDesktopPairing(t *testing.T) {
	desktop := os.Getenv("C0_NATIVE_DESKTOP")
	if desktop == "" {
		t.Skip("set C0_NATIVE_DESKTOP to an isolated fused core fixture")
	}
	if runtime.GOOS != "linux" {
		t.Skip("this fixture launcher is Linux-only; native macOS/Windows receipts remain required")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"owned", "attach"} {
		t.Run(mode, func(t *testing.T) {
			root, err := os.MkdirTemp(os.TempDir(), "c0-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("isolated native evidence retained at %s", root)
					return
				}
				_ = os.RemoveAll(root)
			})
			if err := os.MkdirAll(filepath.Join(root, ".ao"), 0700); err != nil {
				t.Fatal(err)
			}
			if mode == "attach" {
				ownerCmd := exec.Command(desktop, "--c0-owned", root, "--c0-owner-exit")
				ownerCmd.Env = append(os.Environ(), "HOME="+root)
				done := startNativeFixtureProcess(t, ownerCmd, filepath.Join(root, ".ao", "owner.log"))
				select {
				case <-done:
				case <-time.After(90 * time.Second):
					t.Fatal("original owner did not exit")
				}
				data, err := os.ReadFile(filepath.Join(root, ".ao", "running.json"))
				var run struct {
					PID int `json:"pid"`
				}
				if err != nil || json.Unmarshal(data, &run) != nil || run.PID <= 0 {
					t.Fatal("original owner left no fixture daemon")
				}
				process, err := os.FindProcess(run.PID)
				if err != nil {
					t.Fatal(err)
				}
				// The PID came from this private fixture's run file. Verify its image
				// before retaining Go's process handle; no process-name or port kills.
				actual, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", run.PID))
				if err != nil || actual != self {
					_ = process.Release()
					t.Fatal("unexpected detached fixture daemon")
				}
				t.Cleanup(func() {
					_ = process.Signal(syscall.SIGTERM)
					for i := 0; i < 100; i++ {
						if err := process.Signal(syscall.Signal(0)); err != nil {
							break
						}
						time.Sleep(50 * time.Millisecond)
					}
					_ = process.Kill()
					_ = process.Release()
				})
			}
			cmd := exec.Command(desktop, "--c0-"+mode, root, "--c0-review")
			cmd.Env = append(os.Environ(), "HOME="+root)
			startNativeFixtureProcess(t, cmd, filepath.Join(root, ".ao", "desktop.log"))
			deadline := time.Now().Add(90 * time.Second)
			for time.Now().Before(deadline) {
				data, err := os.ReadFile(filepath.Join(root, ".ao", "status.json"))
				var state nativeFixtureState
				if err == nil && json.Unmarshal(data, &state) == nil && state.AuthorityGo && state.MaterialSafe && state.ChallengeScanned {
					if _, err := os.Stat(filepath.Join(root, ".ao", "review-open")); err != nil {
						continue
					}
					if state.ActionState != domain.OperatorActionPrepared {
						t.Fatal("pairing automatically confirmed an action")
					}
					t.Logf("real native %s pairing and open Review qualified; plan remains prepared; nonempty challenge material scan passed", mode)
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Fatalf("real native %s pairing did not qualify; inspect isolated logs at %s", mode, root)
		})
	}
}

func startNativeFixtureProcess(t *testing.T, cmd *exec.Cmd, logPath string) <-chan struct{} {
	t.Helper()
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); _ = log.Close(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return done
}

func TestC0NativeFixtureDaemon(t *testing.T) {
	if os.Getenv("C0_NATIVE_CHILD") != "1" {
		t.Skip("private native fixture child only")
	}
	owner := CaptureOwner()
	defer func() { _ = owner.Close() }()
	root := os.Getenv("C0_NATIVE_ROOT")
	if root == "" {
		t.Fatal("missing isolated root")
	}
	stateRoot := filepath.Join(root, ".ao")
	var token string
	if os.Getenv("C0_NATIVE_MODE") == "owned" {
		credentials, err := appdiagnostics.ReadCredentials(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		token = credentials.OperatorToken
		if token == "" {
			t.Fatal("missing private inherited token")
		}
	} // A directly launched standalone attach daemon has no owned token.

	s, err := sqlite.Open(filepath.Join(stateRoot, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	workspace := filepath.Join(stateRoot, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "project", Path: workspace, RegisteredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	plan := planFor(domain.ConsentSettingsPermissions)
	plan.ValidUntil = time.Now().UTC().Add(10 * time.Minute).Truncate(time.Millisecond)
	planPath := filepath.Join(workspace, "plan.json")
	if data, err := os.ReadFile(planPath); err == nil {
		if err := json.Unmarshal(data, &plan); err != nil {
			t.Fatal(err)
		}
	} else if os.IsNotExist(err) {
		data, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(planPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal(err)
	}
	if _, err := s.PrepareOperatorAction(ctx, plan); err != nil {
		t.Fatal(err)
	}
	b := New(s.NativeOperatorIssuer(), token, owner)
	browser := browserruntime.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "not-a-c0-credential")
	browser.SetNativeHandler(b.Handle)
	ln, err := net.Listen("unix", filepath.Join(stateRoot, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	run, _ := json.Marshal(map[string]any{"address": ln.Addr().String(), "pid": os.Getpid()})
	if err := os.WriteFile(filepath.Join(stateRoot, "running.json"), run, 0600); err != nil {
		t.Fatal(err)
	}
	go func() { _ = browser.Serve(ctx, ln) }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			readiness := b.GetAuthorityReadiness([]domain.OperatorPurpose{plan.Purpose}, plan.Scope)
			receipt, err := s.GetOperatorAction(ctx, plan.ActionKey)
			if err != nil {
				continue
			}
			materials := []string{token}
			b.mu.Lock()
			for _, challenge := range b.challenges {
				materials = append(materials, challenge.value)
			}
			scanned := len(b.challenges) > 0
			b.mu.Unlock()
			data, _ := json.Marshal(nativeFixtureState{ChallengeScanned: scanned, AuthorityGo: readiness.AuthorityGo, IssuerEpoch: readiness.IssuerEpoch, ActionState: receipt.State, MaterialSafe: nativeMaterialsAbsent(root, materials) && nativePeerEnvironmentSafe(b, materials)})
			tmp := filepath.Join(stateRoot, "status.tmp")
			if os.WriteFile(tmp, data, 0600) == nil {
				_ = os.Rename(tmp, filepath.Join(stateRoot, "status.json"))
			}
		}
	}
}

func nativeMaterialsAbsent(root string, materials []string) bool {
	for _, entry := range os.Environ() {
		for _, material := range materials {
			if material != "" && bytes.Contains([]byte(entry), []byte(material)) {
				return false
			}
		}
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, material := range materials {
			if material != "" && bytes.Contains(data, []byte(material)) {
				return errUnqualified
			}
		}
		return nil
	})
	return err == nil
}
