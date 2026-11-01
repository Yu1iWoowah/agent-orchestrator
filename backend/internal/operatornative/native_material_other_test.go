//go:build !linux

package operatornative

// Runtime native qualification on these platforms needs its own receipt.
func nativePeerEnvironmentSafe(*Broker, []string) bool { return false }
