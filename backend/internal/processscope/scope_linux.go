//go:build linux

package processscope

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Capture returns no partial identity: missing evidence remains conservative.
func Capture(providerPID int) *Scope {
	host := os.Getpid()
	pgid, err := unix.Getpgid(providerPID)
	if err != nil || pgid != providerPID {
		return nil
	}
	hostBirth, err := birth(host)
	if err != nil {
		return nil
	}
	leaderBirth, err := birth(pgid)
	if err != nil {
		return nil
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil
	}
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return nil
	}
	return &Scope{HostPID: host, HostBirth: hostBirth, ProviderPGID: pgid, LeaderPID: pgid, LeaderBirth: leaderBirth, BootID: strings.TrimSpace(string(boot)), PIDNamespace: ns}
}

func birth(pid int) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// comm may contain spaces and parentheses. starttime is field 22.
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(raw)[end+1:])
	if len(fields) <= 19 {
		return "", fmt.Errorf("missing process birth")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", err
	}
	return fields[19], nil
}
