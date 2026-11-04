//go:build !linux

package processscope

func Capture(int) *Scope { return nil }
