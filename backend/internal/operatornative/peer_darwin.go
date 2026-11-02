//go:build darwin

package operatornative

import (
	"bytes"
	"net"
	"os"
	"unsafe"

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
	var pid int
	var probeErr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil || cred == nil || int(cred.Uid) != os.Geteuid() {
			probeErr = errUnqualified
			return
		}
		pid, probeErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return "", err
	}
	if probeErr != nil || pid <= 0 {
		return "", errUnqualified
	}
	// proc_info(PROC_PIDPATHINFO) resolves the executable vnode. Process argv
	// is not an executable identity. This is proc_pidpath's underlying syscall.
	const procInfoCallPIDInfo = 2
	const procPIDPathInfo = 11
	data := make([]byte, 4096)
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDPathInfo, 0, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	if errno != 0 {
		return "", errUnqualified
	}
	end := bytes.IndexByte(data, 0)
	if end <= 0 {
		return "", errUnqualified
	}
	return string(data[:end]), nil
}
