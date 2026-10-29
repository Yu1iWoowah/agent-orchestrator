package agentaccount

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestHistoryFIFOIsRefusedWithoutBlocking(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	home := filepath.Join(root, "codex", "a")
	c := &Codex{}
	if err := c.Prepare(home); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "history", "codex", "history.jsonl")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(target, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Prepare(home) }()
	select {
	case err := <-done:
		if !errors.Is(err, ports.ErrAgentAccountStorageUnsafe) {
			t.Fatalf("unsafe FIFO error=%v", err)
		}
	case <-time.After(250 * time.Millisecond):
		fd, err := unix.Open(target, unix.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("fixture cleanup failed")
		}
		t.Fatal("Prepare blocked opening history.jsonl FIFO instead of rejecting its type")
	}
}
