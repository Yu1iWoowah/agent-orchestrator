//go:build !linux

package review

import "os"

func killProcess(captured capturedProcess) error {
	process, err := os.FindProcess(captured.pid)
	if err != nil {
		return err
	}
	// SIGKILL on Unix and TerminateProcess on Windows.
	return process.Kill()
}
