//go:build linux

package operatornative

import (
	"bytes"
	"net"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func nativePeerEnvironmentSafe(b *Broker, materials []string) bool {
	b.mu.Lock()
	conn, ok := b.conn.(*net.UnixConn)
	b.mu.Unlock()
	if !ok {
		return false
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var probeErr error
	if raw.Control(func(fd uintptr) { cred, probeErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }) != nil || probeErr != nil || cred == nil {
		return false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(int(cred.Pid)) + "/environ")
	if err != nil {
		return false
	}
	for _, material := range materials {
		if material != "" && bytes.Contains(data, []byte(material)) {
			return false
		}
	}
	return true
}
