//go:build !linux && !darwin

package filefingerprint

import "os"

// No stat-only fallback: Windows creation time is not a change-time witness.
// Publication must fail closed until a native change-time source is verified.
func changeStamp(_ os.FileInfo) (fileStamp, bool) {
	return fileStamp{}, false
}
