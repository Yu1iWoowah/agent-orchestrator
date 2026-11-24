//go:build !linux && !windows

package review

import "syscall"

// freezeProcess stops a process with SIGSTOP. A stopped process cannot spawn
// anything, and SIGKILL still ends it.
func freezeProcess(process capturedProcess) error {
	return syscall.Kill(process.pid, syscall.SIGSTOP)
}
