//go:build windows

package operatornative

import (
	"net"

	"golang.org/x/sys/windows"
)

func nativePeerExecutable(conn net.Conn) (string, error) {
	pipe, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return "", errUnqualified
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(pipe.Fd()), &pid); err != nil || pid == 0 {
		return "", errUnqualified
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", errUnqualified
	}
	defer windows.CloseHandle(handle)
	var token windows.Token
	if windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token) != nil {
		return "", errUnqualified
	}
	defer token.Close()
	peer, err := token.GetTokenUser()
	if err != nil {
		return "", errUnqualified
	}
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || !peer.User.Sid.Equals(owner.User.Sid) {
		return "", errUnqualified
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size) != nil {
		return "", errUnqualified
	}
	return windows.UTF16ToString(buffer[:size]), nil
}
