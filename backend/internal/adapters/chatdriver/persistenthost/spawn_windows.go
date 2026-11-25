//go:build windows

package persistenthost

import (
	"context"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
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
	cmd := exec.Command(exe, args...) //nolint:forbidigo // detached chat host has specialized lifetime and creation flags
	cmd.Dir = cfg.Workdir
	cmd.Env = cfg.Env
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
	return startDetachedHost(cmd, cfg)
}

func releaseDetachedHost(cmd *exec.Cmd) error {
	return cmd.Process.Release()
}
