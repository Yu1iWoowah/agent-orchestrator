//go:build !linux && !darwin && !windows

package operatornative

import "net"

func nativePeerExecutable(net.Conn) (string, error) { return "", errUnqualified }
