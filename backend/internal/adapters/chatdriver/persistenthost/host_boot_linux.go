//go:build linux

package persistenthost

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// readBootID names the current boot; it never repeats across reboots and does
// not move with the wall clock.
var readBootID = func() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", errors.New("empty boot id")
	}
	return id, nil
}

// readBootTime is the wall-clock time of the current boot (/proc/stat btime).
var readBootTime = func() (time.Time, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(b))
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), "btime "); ok {
			seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(seconds, 0), nil
		}
	}
	return time.Time{}, errors.New("btime missing from /proc/stat")
}
