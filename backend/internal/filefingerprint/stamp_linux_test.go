package filefingerprint

import (
	"os"
	"syscall"
	"testing"
)

type suppliedNativeStat struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (s suppliedNativeStat) Sys() any { return s.stat }

func TestChangeStampPreservesDeviceAndNanoseconds(t *testing.T) {
	stat := &syscall.Stat_t{Dev: 7}
	stat.Ctim.Sec, stat.Ctim.Nsec = 42, 123
	info := suppliedNativeStat{stat: stat}
	first, ok := changeStamp(info)
	if !ok || first.device != 7 || first.ctimeSeconds != 42 || first.ctimeNanoseconds != 123 {
		t.Fatalf("native stamp lost fields: %+v available=%t", first, ok)
	}
	stat.Ctim.Nsec++
	next, ok := changeStamp(info)
	if !ok || first == next {
		t.Fatal("same-second change time was truncated")
	}
	stat.Ctim.Nsec--
	stat.Dev++
	next, ok = changeStamp(info)
	if !ok || first == next {
		t.Fatal("device identity was lost")
	}
}
