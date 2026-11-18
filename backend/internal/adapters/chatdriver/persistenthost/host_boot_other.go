//go:build !linux

package persistenthost

import (
	"errors"
	"time"
)

// Other platforms record no boot identity: hostFromPreviousBoot never proves
// a host gone there and the existing liveness rules apply unchanged.
var readBootID = func() (string, error) { return "", errors.New("boot id unavailable on this platform") }

var readBootTime = func() (time.Time, error) {
	return time.Time{}, errors.New("boot time unavailable on this platform")
}
