package delegate

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Every HCG reader/writer locks byte zero on its own handle. Close releases it.
func tryBudgetLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return err == nil, err
}

const budgetLockFlags = os.O_RDWR | os.O_CREATE

func syncBudgetDirectory(string) error { return nil }
