package agentaccounts

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// recoverUnavailableNamespace removes only the empty, owned reserved skeleton.
// Unknown entries are user data until proven otherwise: no file is opened or
// unlinked. All ancestry is opened without following links, and rmdir itself
// refuses a directory populated after preflight.
func recoverUnavailableNamespace(root string) (bool, error) {
	blocked := ports.ErrAgentAccountUnavailableHomeOccupied
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
		return false, ports.ErrAgentAccountStorageUnsafe
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return false, accountProfileStorageError(e)
	}
	defer func() { _ = unix.Close(fd) }()
	parts := strings.Split(strings.TrimPrefix(root, "/"), "/")
	current := "/"
	for i := 0; ; i++ {
		var st unix.Stat_t
		if e := unix.Fstat(fd, &st); e != nil {
			return false, accountProfileStorageError(e)
		}
		owner := st.Uid == uint32(unix.Geteuid()) //nolint:gosec // Linux uid_t uses the same UID.
		sticky := st.Uid == 0 && st.Mode&unix.S_ISVTX != 0
		if st.Uid != 0 && !owner || st.Mode&0o022 != 0 && !sticky || current == root && (!owner || st.Mode&0o777 != 0o700) {
			return false, ports.ErrAgentAccountStorageUnsafe
		}
		if i == len(parts) {
			break
		}
		next, e := unix.Openat(fd, parts[i], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(e, unix.ENOENT) {
			return false, nil
		}
		if e != nil {
			return false, accountProfileStorageError(e)
		}
		_ = unix.Close(fd)
		fd = next
		current = filepath.Join(current, parts[i])
	}
	reserved, e := unix.Openat(fd, ".unavailable", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(e, unix.ENOENT) {
		return false, nil
	}
	if e != nil {
		return false, blocked
	}
	defer func() { _ = unix.Close(reserved) }()
	if !ownedUnavailableDirectory(reserved) {
		return false, blocked
	}
	names, e := unavailableDirectoryNames(reserved, 3)
	if e != nil || len(names) > 2 {
		return false, blocked
	}
	type candidate struct {
		name string
		fd   int
	}
	var plan []candidate
	defer func() {
		for _, c := range plan {
			_ = unix.Close(c.fd)
		}
	}()
	for _, name := range names {
		if name != "claude" && name != "codex" {
			return false, blocked
		}
		child, e := unix.Openat(reserved, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return false, blocked
		}
		plan = append(plan, candidate{name, child})
		if !ownedUnavailableDirectory(child) {
			return false, blocked
		}
		entries, e := unavailableDirectoryNames(child, 1)
		if e != nil || len(entries) != 0 {
			return false, blocked
		}
	}
	// Preflight every directory before changing any of them. Recheck identity at
	// each unlink, and only remove empty directories, never credential/cache files.
	for _, c := range plan {
		if !sameUnavailableDirectory(fd, ".unavailable", reserved) || !sameUnavailableDirectory(reserved, c.name, c.fd) {
			return false, blocked
		}
		if e := unix.Unlinkat(reserved, c.name, unix.AT_REMOVEDIR); e != nil {
			return false, blocked
		}
	}
	if !sameUnavailableDirectory(fd, ".unavailable", reserved) {
		return false, blocked
	}
	if e := unix.Unlinkat(fd, ".unavailable", unix.AT_REMOVEDIR); e != nil {
		return false, blocked
	}
	return true, nil
}
func ownedUnavailableDirectory(fd int) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Uid == uint32(unix.Geteuid()) //nolint:gosec // Linux uid_t uses the same UID.
}
func sameUnavailableDirectory(parent int, name string, fd int) bool {
	var opened, current unix.Stat_t
	return unix.Fstat(fd, &opened) == nil && unix.Fstatat(parent, name, &current, unix.AT_SYMLINK_NOFOLLOW) == nil && current.Mode&unix.S_IFMT == unix.S_IFDIR && opened.Dev == current.Dev && opened.Ino == current.Ino
}
func unavailableDirectoryNames(fd, limit int) ([]string, error) {
	dup, e := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(dup), "unavailable-namespace")
	entries, e := f.ReadDir(limit)
	_ = f.Close()
	if e != nil && !errors.Is(e, io.EOF) {
		return nil, e
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}
