//go:build linux || darwin

package rca

import (
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

func currentVolume(root *os.Root) (volumeSpace, error) {
	f, err := root.Open(".")
	if err != nil {
		return volumeSpace{}, err
	}
	defer func() { _ = f.Close() }()
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &stat); err != nil {
		return volumeSpace{}, err
	}
	if stat.Bsize <= 0 || stat.Blocks == 0 {
		return volumeSpace{}, errors.New("invalid volume values")
	}
	unit, blocks, available := uint64(stat.Bsize), stat.Blocks, stat.Bavail
	if blocks > math.MaxUint64/unit || available > blocks {
		return volumeSpace{}, errors.New("invalid volume capacity")
	}
	return volumeSpace{Total: blocks * unit, Available: available * unit, Identity: fmt.Sprint(stat.Fsid)}, nil
}
