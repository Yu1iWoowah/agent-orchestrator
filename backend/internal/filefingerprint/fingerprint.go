// Package filefingerprint compares regular-file identity and native change time
// after a caller has verified file contents outside a writer reservation.
package filefingerprint

import "os"

type fileStamp struct {
	device           uint64
	ctimeSeconds     int64
	ctimeNanoseconds int64
}

// Fingerprint is an opaque observation of one verified regular file.
type Fingerprint struct {
	info  os.FileInfo
	stamp fileStamp
}

// Capture requires a native change-time and device witness. Unsupported
// platforms return false; callers must fail closed rather than use size/mtime.
func Capture(info os.FileInfo) (Fingerprint, bool) {
	if info == nil || !info.Mode().IsRegular() {
		return Fingerprint{}, false
	}
	stamp, ok := changeStamp(info)
	if !ok {
		return Fingerprint{}, false
	}
	return Fingerprint{info: info, stamp: stamp}, true
}

// Matches compares identity, size, modification time and native change time.
// It does no I/O; callers stat both the opened file and its confined name.
func (f Fingerprint) Matches(info os.FileInfo) bool {
	if f.info == nil || info == nil || !info.Mode().IsRegular() {
		return false
	}
	stamp, ok := changeStamp(info)
	return ok && stamp == f.stamp && os.SameFile(f.info, info) && info.Size() == f.info.Size() && info.ModTime().Equal(f.info.ModTime())
}
