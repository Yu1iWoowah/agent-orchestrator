package filefingerprint

import (
	"os"
	"syscall"
)

func changeStamp(info os.FileInfo) (fileStamp, bool) {
	if info == nil {
		return fileStamp{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fileStamp{}, false
	}
	seconds, nanoseconds := stat.Ctimespec.Unix()
	return fileStamp{device: uint64(stat.Dev), ctimeSeconds: seconds, ctimeNanoseconds: nanoseconds}, true
}
