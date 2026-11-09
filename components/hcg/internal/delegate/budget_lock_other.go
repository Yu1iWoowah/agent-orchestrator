//go:build !unix && !windows

package delegate

import (
	"fmt"
	"os"
)

func tryBudgetLock(*os.File) (bool, error) {
	return false, fmt.Errorf("delegation budget locking unsupported")
}

const budgetLockFlags = os.O_RDWR | os.O_CREATE

func syncBudgetDirectory(string) error { return ErrBudgetBlocked }
