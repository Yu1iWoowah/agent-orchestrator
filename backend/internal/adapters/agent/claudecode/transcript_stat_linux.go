package claudecode

import (
	"os"
	"syscall"
)

func nativeFileChangeStamp(info os.FileInfo) (nativeFileStamp, bool) {
	if info == nil {
		return nativeFileStamp{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return nativeFileStamp{}, false
	}
	seconds, nanoseconds := stat.Ctim.Unix()
	return nativeFileStamp{
		device:           uint64(stat.Dev), //nolint:unconvert // Dev is uint32 on Linux MIPS and uint64 on other targets.
		ctimeSeconds:     seconds,
		ctimeNanoseconds: nanoseconds,
	}, true
}
