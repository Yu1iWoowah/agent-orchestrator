//go:build !linux

package processscope

import "context"

// Capture leaves process identity absent on unsupported platforms.
func Capture(int) *Scope { return nil }

// Ended cannot establish a process boundary on this platform.
func Ended(context.Context, *Scope) (bool, string) { return false, "scope_platform_unsupported" }
