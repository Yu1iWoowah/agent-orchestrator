//go:build !linux

package processscope

import "context"

func AdapterExited(*Scope) bool { return false }

// StartTicks is unavailable on platforms without Linux procfs.
func StartTicks(int) string { return "" }

// Capture leaves process identity absent on unsupported platforms.
func Capture(int) *Scope { return nil }

// Ended cannot establish a process boundary on this platform.
func Ended(context.Context, *Scope) (bool, string) { return false, "scope_platform_unsupported" }

// CurrentScope leaves the process comparison boundary unset on unsupported platforms.
func CurrentScope() (bootID, pidNamespace string) { return "", "" }
