//go:build !linux

package operatornative

// CaptureOwner cannot qualify an unverified platform for tokenless attach.
func CaptureOwner() *Owner { return &Owner{} }
