//go:build !unix

package delegate

import "os"

// Private reply records require a POSIX owner check; elsewhere they are unavailable.
func ownedByProcess(os.FileInfo) bool { return false }
