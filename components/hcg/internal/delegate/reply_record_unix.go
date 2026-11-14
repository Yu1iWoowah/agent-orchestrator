//go:build unix

package delegate

import (
	"os"
	"syscall"
)

func ownedByProcess(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
