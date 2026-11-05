//go:build linux

package processscope

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

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
