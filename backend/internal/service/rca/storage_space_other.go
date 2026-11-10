//go:build !linux && !darwin

package rca

import (
	"errors"
	"os"
)

func currentVolume(*os.Root) (volumeSpace, error) {
	return volumeSpace{}, errors.New("volume identity unavailable")
}
