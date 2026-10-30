//go:build !windows

package persistenthost

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

func configureProviderProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func startProviderProcess(cmd *exec.Cmd) (func(bool) error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func(verify bool) error {
		// This proves only the AO-owned provider PGID. Tools that deliberately
		// create another process group are outside this runtime boundary, as are
		// workers and user services; replacement fences the retired AO owner and
		// never broadens cleanup into a process-tree scan or a global kill.
		if !verify {
			return nil
		}
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			err := syscall.Kill(-cmd.Process.Pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				return nil
			}
			if err != nil && !errors.Is(err, syscall.EPERM) {
				return err
			}
			select {
			case <-deadline.C:
				return ErrOwnershipInconclusive
			case <-tick.C:
			}
		}
	}, nil
}

// killProviderProcess asks the provider's process group to exit before it
// kills it, so the agent can stop the commands it runs in groups of their own.
func killProviderProcess(_ context.Context, cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return aoprocess.TerminateGroup(cmd.Process.Pid, 2*time.Second)
}

// terminateProviderProcess asks the hosted process group to exit.
func terminateProviderProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
