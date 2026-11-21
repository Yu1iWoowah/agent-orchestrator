//go:build !linux

package procmem

// AllThreadsStopped cannot establish Linux group-stop evidence on this platform.
func AllThreadsStopped(int, string) bool { return false }
