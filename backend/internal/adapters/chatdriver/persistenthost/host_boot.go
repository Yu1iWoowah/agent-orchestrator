package persistenthost

import "time"

// currentBootID is recorded in host.json and host.lock; empty when unknown.
func currentBootID() string {
	id, err := readBootID()
	if err != nil {
		return ""
	}
	return id
}

// hostFromPreviousBoot reports positive proof that a recorded host belongs to
// an earlier boot, so a live process with its PID cannot be that host.
// Unknown boot identity is never proof.
func hostFromPreviousBoot(bootID string, startedAt time.Time) bool {
	current, err := readBootID()
	if err != nil {
		return false
	}
	if bootID != "" {
		return bootID != current
	}
	// u3 transition (user sdr-total-review-1 q5=b): hosts started before
	// boot ids were recorded carry only StartedAt. Delete this branch once no
	// u3-started host can remain (they all restart at the next reboot).
	if startedAt.IsZero() {
		return false
	}
	booted, err := readBootTime()
	return err == nil && startedAt.Before(booted)
}
