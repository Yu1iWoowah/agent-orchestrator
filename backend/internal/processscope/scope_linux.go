//go:build linux

package processscope

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Ended inspects the original scope without signalling it to stop.
func Ended(ctx context.Context, scope *Scope) (bool, string) {
	return inspect(ctx, scope, os.ReadFile, os.Readlink, birth, unix.Kill)
}

func inspect(ctx context.Context, scope *Scope, readFile func(string) ([]byte, error), readlink func(string) (string, error), processBirth func(int) (string, error), kill func(int, unix.Signal) error) (bool, string) {
	if ctx.Err() != nil {
		return false, "scope_probe_timeout"
	}
	if scope == nil || scope.HostPID <= 0 || scope.ProviderPGID <= 0 || scope.LeaderPID != scope.ProviderPGID || scope.HostBirth == "" || scope.LeaderBirth == "" || scope.BootID == "" || scope.PIDNamespace == "" {
		return false, "scope_identity_missing"
	}
	boot, err := readFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) == "" {
		return false, "scope_boot_unavailable"
	}
	if strings.TrimSpace(string(boot)) != scope.BootID {
		return true, "scope_boot_changed"
	}
	ns, err := readlink("/proc/self/ns/pid")
	if err != nil || ns != scope.PIDNamespace {
		return false, "scope_namespace_mismatch"
	}
	hostBirth, err := processBirth(scope.HostPID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, "scope_host_probe_failed"
	}
	if err == nil && hostBirth == scope.HostBirth {
		return false, "scope_host_alive"
	}
	err = kill(-scope.ProviderPGID, 0)
	if ctx.Err() != nil {
		return false, "scope_probe_timeout"
	}
	if errors.Is(err, unix.ESRCH) {
		return true, "scope_host_gone_group_esrch"
	}
	if err == nil {
		return false, "scope_group_alive"
	}
	return false, "scope_group_probe_failed"
}

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
