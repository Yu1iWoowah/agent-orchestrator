//go:build unix

package delegate

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Locks belong to the open file description and are released by Close.
func tryBudgetLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

const budgetLockFlags = os.O_RDWR | os.O_CREATE | unix.O_NOFOLLOW | unix.O_NONBLOCK

func syncBudgetDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return ErrBudgetBlocked
	}
	defer f.Close()
	if f.Sync() != nil {
		return ErrBudgetBlocked
	}
	return nil
}
