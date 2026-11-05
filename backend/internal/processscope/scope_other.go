//go:build !linux

package processscope

// Capture leaves process identity absent on unsupported platforms.
func Capture(int) *Scope { return nil }
