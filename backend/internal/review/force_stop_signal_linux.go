package review

import (
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
)

func freezeProcess(process capturedProcess) error {
	return signalCapturedProcess(process, unix.SIGSTOP)
}

func killProcess(process capturedProcess) error {
	return signalCapturedProcess(process, unix.SIGKILL)
}

func signalCapturedProcess(process capturedProcess, signal unix.Signal) error {
	fd, err := unix.PidfdOpen(process.pid, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()

	// Open the pidfd before verifying the start time. If the process exits
	// after verification, the fd still refers to that process.
	if processscope.StartTicks(process.pid) != process.start {
		return fmt.Errorf("cannot verify start time of reviewer process %d", process.pid)
	}
	return unix.PidfdSendSignal(fd, signal, nil, 0)
}
