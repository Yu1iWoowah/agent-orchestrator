//go:build unix

package delegate

import (
	"golang.org/x/sys/unix"
	"os"
)

const sourceOpenFlags = os.O_RDONLY | unix.O_NONBLOCK
