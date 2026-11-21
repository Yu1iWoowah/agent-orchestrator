//go:build !unix && !windows

package quota

import (
	"fmt"
	"os"
)

func tryLogLock(*os.File) (bool, error) { return false, fmt.Errorf("quota log locking unsupported") }
