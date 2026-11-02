//go:build !linux

package operatornative

import "os"

// Non-Linux qualification remains disabled pending native runtime receipts.
func openDigestFile(path string) (*os.File, error) { return os.Open(path) }
