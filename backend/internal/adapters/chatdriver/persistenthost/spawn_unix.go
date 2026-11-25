//go:build !windows

package persistenthost

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

func spawnDetached(ctx context.Context, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, args, err := hostExecutable(exe, hostArgs(cfg))
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, args...) //nolint:forbidigo // detached chat host has specialized lifetime and session ownership
	cmd.Dir = cfg.Workdir
	cmd.Env = cfg.Env
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return startDetachedHost(cmd, cfg)
}

func releaseDetachedHost(cmd *exec.Cmd) error {
	// Setsid detaches the host from our session. Wait still reaps our child.
	go func() { _ = cmd.Wait() }()
	return nil
}
