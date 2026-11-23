package persistenthost

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/processalive"
)

// ProviderOwnership describes what the host records establish about the provider.
type ProviderOwnership int

const (
	// ProviderOwnershipUncertain includes same-boot owners, even if the host died.
	ProviderOwnershipUncertain ProviderOwnership = iota
	// ProviderOwnershipUnrecorded means no descriptor, lock or spawn record exists.
	ProviderOwnershipUnrecorded
	// ProviderOwnershipPreviousBoot identifies records from a previous boot.
	ProviderOwnershipPreviousBoot
)

// ProbeProviderOwnership reads the records without contacting or starting a host.
// A provider runs in its own process group and can outlive its host on this boot.
func ProbeProviderOwnership(dataDir, sessionID string) ProviderOwnership {
	path, err := lockPath(dataDir, sessionID)
	if err != nil {
		return ProviderOwnershipUncertain
	}
	pids, spawnState := spawnRecords(dataDir, sessionID)
	if spawnState == HostUnknown {
		return ProviderOwnershipUncertain
	}
	lock, lockErr := os.ReadFile(path) //nolint:gosec // AO-owned path derived from validated session id.
	d, descriptorErr := readDescriptorOnce(dataDir, sessionID)
	noLock := errors.Is(lockErr, os.ErrNotExist)
	if noLock && errors.Is(descriptorErr, os.ErrNotExist) && len(pids) == 0 {
		return ProviderOwnershipUnrecorded
	}
	if descriptorErr != nil || d.PID <= 0 || d.StartedAt.IsZero() || !EpochFromPreviousBoot(deliveryEpoch(d)) {
		return ProviderOwnershipUncertain
	}
	if !noLock {
		fields := strings.Fields(string(lock))
		if lockErr != nil || len(fields) == 0 || len(fields) > 2 || fields[0] != strconv.Itoa(d.PID) ||
			(len(fields) == 2 && fields[1] != d.BootID) {
			return ProviderOwnershipUncertain
		}
	}
	// Spawn markers have no boot ID. A different PID or a live one can be a new
	// host, including one reusing the previous boot's PID.
	for _, pid := range pids {
		if pid != d.PID || processalive.Alive(pid) {
			return ProviderOwnershipUncertain
		}
	}
	return ProviderOwnershipPreviousBoot
}
