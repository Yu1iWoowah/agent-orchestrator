package operatornative

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	AuthorityGo  bool                       `json:"authorityGo"`
	IssuerEpoch  string                     `json:"issuerEpoch"`
	ActionState  domain.OperatorActionState `json:"actionState"`
	MaterialSafe bool                       `json:"materialSafe"`
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
				cmd := exec.Command(self, "-test.run=^TestC0NativeFixtureDaemon$", "-test.timeout=5m")
				cmd.Env = append(os.Environ(), "HOME="+root, "C0_NATIVE_CHILD=1", "C0_NATIVE_ROOT="+root, "C0_NATIVE_MODE=attach")
				startNativeFixtureProcess(t, cmd, filepath.Join(root, ".ao", "daemon.log"))
			}
			cmd := exec.Command(desktop, "--c0-"+mode, root)
			cmd.Env = append(os.Environ(), "HOME="+root)
			startNativeFixtureProcess(t, cmd, filepath.Join(root, ".ao", "desktop.log"))
			deadline := time.Now().Add(90 * time.Second)
			for time.Now().Before(deadline) {
				data, err := os.ReadFile(filepath.Join(root, ".ao", "status.json"))
				var state nativeFixtureState
				if err == nil && json.Unmarshal(data, &state) == nil && state.AuthorityGo && state.MaterialSafe {
					if state.ActionState != domain.OperatorActionPrepared {
						t.Fatal("pairing automatically confirmed an action")
					}
					t.Logf("real native %s pairing qualified; plan remains prepared; material scan passed", mode)
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Fatalf("real native %s pairing did not qualify; inspect isolated logs at %s", mode, root)
		})
	}
}

func startNativeFixtureProcess(t *testing.T, cmd *exec.Cmd, logPath string) {
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
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); _ = log.Close() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
}

func TestC0NativeFixtureDaemon(t *testing.T) {
	if os.Getenv("C0_NATIVE_CHILD") != "1" {
		t.Skip("private native fixture child only")
	}
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
	} else {
		// Model a daemon retaining its previous owner's private credential.
		// The new desktop never receives this value and still must pair natively.
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			t.Fatal(err)
		}
		token = hex.EncodeToString(secret)
	}
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
	b := New(s, token)
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
			b.mu.Unlock()
			data, _ := json.Marshal(nativeFixtureState{AuthorityGo: readiness.AuthorityGo, IssuerEpoch: readiness.IssuerEpoch, ActionState: receipt.State, MaterialSafe: nativeMaterialsAbsent(root, materials) && nativePeerEnvironmentSafe(b, materials)})
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
