//go:build unix

package integration

import (
	"golang.org/x/sys/unix"
	"os"
)

// Check the opened descriptor without blocking on a substituted FIFO.
const sourceOpenFlags = os.O_RDONLY | unix.O_NONBLOCK
