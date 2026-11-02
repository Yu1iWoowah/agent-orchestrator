//go:build linux

package operatornative

import (
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

func watchOwnerPID(pid int) *Owner {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return &Owner{}
	}
	if fd < 0 || fd > 1<<31-1 {
		_ = unix.Close(fd)
		return &Owner{}
	}
	pollFD := int32(fd)
	return &Owner{
		exited: func() (bool, error) {
			fds := []unix.PollFd{{Fd: pollFD, Events: unix.POLLIN}}
			_, err := unix.Poll(fds, 0)
			if err != nil {
				return false, err
			}
			if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
				return false, errUnqualified
			}
			return fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
		},
		release: func() error { return unix.Close(fd) },
	}
}

// CaptureOwner runs at daemon entry, before migrations or stdin reads. The
// production bundled launch is a direct child of Electron main. Custom shell
// wrappers cannot stand in for that process. The pidfd survives PID reuse.
func CaptureOwner() *Owner {
	pid := os.Getppid()
	if pid <= 1 {
		return &Owner{}
	}
	owner := watchOwnerPID(pid)
	self, err := os.Executable()
	if err != nil {
		_ = owner.Close()
		return &Owner{}
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		_ = owner.Close()
		return &Owner{}
	}
	expected, err := packagedDesktop(self)
	actual, probeErr := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil || probeErr != nil || filepath.Base(actual) != filepath.Base(expected) || os.Getppid() != pid {
		_ = owner.Close()
		return &Owner{}
	}
	if !sameOwnerBundle("/proc/"+strconv.Itoa(pid)+"/exe", desktopArchive(actual), expected, desktopArchive(expected)) {
		_ = owner.Close()
		return &Owner{}
	}

	if os.Getppid() != pid {
		_ = owner.Close()
		return &Owner{}
	}
	return owner
}

func sameOwnerBundle(parentImage, parentArchive, expectedImage, expectedArchive string) bool {
	for _, pair := range [][2]string{{parentImage, expectedImage}, {parentArchive, expectedArchive}} {
		parentFile, parentErr := os.Stat(pair[0])
		bundleFile, bundleErr := os.Stat(pair[1])
		if parentErr != nil || bundleErr != nil || !os.SameFile(parentFile, bundleFile) {
			return false
		}
	}
	return true
}
