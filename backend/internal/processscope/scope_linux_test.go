//go:build linux

package processscope

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
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
