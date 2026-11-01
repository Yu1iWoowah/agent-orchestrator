//go:build linux

package operatornative

import (
	"net"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func nativePeerExecutable(conn net.Conn) (string, error) {
	socket, ok := conn.(*net.UnixConn)
	if !ok {
		return "", errUnqualified
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return "", err
	}
	var cred *unix.Ucred
	var probeErr error
	if err := raw.Control(func(fd uintptr) { cred, probeErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return "", err
	}
	if probeErr != nil || cred == nil || int(cred.Uid) != os.Geteuid() || cred.Pid <= 0 {
		return "", errUnqualified
	}
	return os.Readlink("/proc/" + strconv.Itoa(int(cred.Pid)) + "/exe")
}
