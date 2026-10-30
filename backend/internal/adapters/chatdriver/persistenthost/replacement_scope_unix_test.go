//go:build !windows

package persistenthost

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
)

func TestReplacementUnixStopProofIsScopedToOwnedProcessGroup(t *testing.T) {
	start := func() (*exec.Cmd, func(bool) error) {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		configureProviderProcess(cmd)
		proof, err := startProviderProcess(cmd)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd, proof
	}
	provider, proof := start()
	unrelated, _ := start()
	if err := killProviderProcess(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	_ = provider.Wait()
	if err := proof(true); err != nil {
		t.Fatal("owned group was not positively stopped", err)
	}
	if err := syscall.Kill(unrelated.Process.Pid, 0); err != nil {
		t.Fatal("replacement stopped an unrelated process group", err)
	}
}
