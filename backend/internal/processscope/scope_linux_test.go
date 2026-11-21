//go:build linux

package processscope

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAdapterExitedWaitsForThreadsAfterLeaderBecomesZombie(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler required for the pthread fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "adapter")
	compile := exec.CommandContext(ctx, cc, "-pthread", "-x", "c", "-o", bin, "-")
	compile.Stdin = strings.NewReader(`
#include <pthread.h>
#include <unistd.h>

static void *run(void *unused) {
  char release;
  read(STDIN_FILENO, &release, 1);
  return NULL;
}

int main(void) {
  pthread_t sibling;
  if (pthread_create(&sibling, NULL, run, NULL) != 0) return 2;
  pthread_exit(NULL);
}
`)
	if out, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile pthread fixture: %v\n%s", err, out)
	}
	cmd := exec.CommandContext(ctx, bin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	release, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = release.Close(); _ = cmd.Wait() })
	scope := Capture(cmd.Process.Pid)
	if scope == nil {
		t.Fatal("could not capture adapter identity")
	}
	statPath := fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid)
	for {
		raw, err := os.ReadFile(statPath)
		if err != nil {
			t.Fatal(err)
		}
		// Wait for the exact kernel state under review. Stdin is still open,
		// leaving the second thread blocked and therefore alive.
		fields := strings.Fields(string(raw)[strings.LastIndexByte(string(raw), ')')+1:])
		if len(fields) > 17 && fields[0] == "Z" && fields[17] == "2" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("leader did not become a zombie with a live sibling")
		case <-time.After(time.Millisecond):
		}
	}
	if AdapterExited(scope) {
		t.Fatal("reported adapter exit while the zombie leader still had a live sibling")
	}
	if err := release.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Waitid(unix.P_PID, cmd.Process.Pid, nil, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	if !AdapterExited(scope) {
		t.Fatal("the adapter still appeared alive after all threads exited")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureOriginalProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scope := Capture(cmd.Process.Pid)
	if scope == nil || scope.HostPID != os.Getpid() || scope.ProviderPGID != cmd.Process.Pid || scope.LeaderPID != cmd.Process.Pid || scope.HostBirth == "" || scope.LeaderBirth == "" || scope.BootID == "" || scope.PIDNamespace == "" {
		t.Fatalf("incomplete scope: %+v", scope)
	}
	if Capture(-1) != nil {
		t.Fatal("missing process produced identity")
	}
}

func TestEndedScopeBoundaries(t *testing.T) {
	original := Scope{HostPID: 101, HostBirth: "1", ProviderPGID: 202, LeaderPID: 202, LeaderBirth: "2", BootID: "boot", PIDNamespace: "ns"}
	for _, tc := range []struct {
		name, boot, ns, hostBirth, reason string
		hostErr, groupErr                 error
		missing, canceled, ended          bool
	}{
		{name: "host missing group absent", hostErr: os.ErrNotExist, groupErr: unix.ESRCH, ended: true, reason: "scope_host_gone_group_esrch"},
		{name: "host PID reused", hostBirth: "different", groupErr: unix.ESRCH, ended: true, reason: "scope_host_gone_group_esrch"},
		{name: "reboot", boot: "next-boot", ended: true, reason: "scope_boot_changed"},
		{name: "host alive even idle", hostBirth: "1", groupErr: unix.ESRCH, reason: "scope_host_alive"},
		{name: "group alive with missing leader", hostErr: os.ErrNotExist, reason: "scope_group_alive"},
		{name: "group EPERM", hostErr: os.ErrNotExist, groupErr: unix.EPERM, reason: "scope_group_probe_failed"},
		{name: "host EPERM", hostErr: unix.EPERM, reason: "scope_host_probe_failed"},
		{name: "namespace differs", ns: "other", reason: "scope_namespace_mismatch"},
		{name: "missing identity even after reboot", missing: true, boot: "next-boot", reason: "scope_identity_missing"},
		{name: "timeout", canceled: true, reason: "scope_probe_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := original
			if tc.missing {
				scope.LeaderBirth = ""
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			boot, ns := tc.boot, tc.ns
			if boot == "" {
				boot = "boot"
			}
			if ns == "" {
				ns = "ns"
			}
			ended, reason := inspect(ctx, &scope,
				func(string) ([]byte, error) { return []byte(boot), nil },
				func(string) (string, error) { return ns, nil },
				func(int) (string, error) { return tc.hostBirth, tc.hostErr },
				func(pid int, signal unix.Signal) error {
					if pid != -original.ProviderPGID || signal != 0 {
						t.Fatalf("unsafe probe: %d %v", pid, signal)
					}
					return tc.groupErr
				})
			if ended != tc.ended || reason != tc.reason {
				t.Fatalf("ended=%v reason=%s", ended, reason)
			}
		})
	}
}

func TestEndedRealProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := false
	t.Cleanup(func() {
		if !done {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	scope := Capture(cmd.Process.Pid)
	if scope == nil {
		t.Fatal("scope unavailable")
	}
	if ended, _ := Ended(t.Context(), scope); ended {
		t.Fatal("live host scope ended")
	}
	// A reused host PID does not make a live provider group disappear.
	scope.HostBirth = "reused"
	if ended, reason := Ended(t.Context(), scope); ended || reason != "scope_group_alive" {
		t.Fatalf("%v %s", ended, reason)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	done = true
	if ended, reason := Ended(t.Context(), scope); !ended || reason != "scope_host_gone_group_esrch" {
		t.Fatalf("%v %s", ended, reason)
	}
}
