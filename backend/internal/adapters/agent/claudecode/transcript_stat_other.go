//go:build !linux && !darwin

package claudecode

import "os"

// No stat-only fallback: Windows creation time is not a change-time witness.
// Publication must fail closed until a native change-time source is verified.
func nativeFileChangeStamp(_ os.FileInfo) (nativeFileStamp, bool) {
	return nativeFileStamp{}, false
}
