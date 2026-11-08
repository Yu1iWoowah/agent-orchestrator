//go:build unix

package quota

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Locks belong to the open file description and are released by Close.
func tryLogLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
