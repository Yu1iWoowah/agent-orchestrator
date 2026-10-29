package agentaccounts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ensureAccountProfile opens every ancestor without following links. Existing
// private directories are never repaired: the caller must fix them and retry.
func ensureAccountProfile(root, home string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(home) || filepath.Clean(root) == "/" {
		return ports.ErrAgentAccountStorageUnsafe
	}
	root, home = filepath.Clean(root), filepath.Clean(home)
	rel, err := filepath.Rel(root, home)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ports.ErrAgentAccountStorageUnsafe
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return accountProfileStorageError(err)
	}
	defer func() { _ = unix.Close(fd) }()
	current := "/"
	parts := strings.Split(strings.TrimPrefix(home, "/"), "/")
	for i := 0; ; i++ {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return accountProfileStorageError(err)
		}
		owner := stat.Uid == uint32(unix.Geteuid()) //nolint:gosec // Linux uid_t and Geteuid use the same UID.
		private := current == root || strings.HasPrefix(current, root+"/")
		stickyRoot := stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0
		if (stat.Uid != 0 && !owner) || stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
			(stat.Mode&0o022 != 0 && !stickyRoot) || (private && (!owner || stat.Mode&0o777 != 0o700)) {
			return ports.ErrAgentAccountStorageUnsafe
		}
		if i == len(parts) {
			for _, name := range []string{"auth.json", ".credentials.json"} {
				var credential unix.Stat_t
				if err := unix.Fstatat(fd, name, &credential, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					if errors.Is(err, unix.ENOENT) {
						continue
					}
					return accountProfileStorageError(err)
				}
				if credential.Mode&unix.S_IFMT != unix.S_IFREG || credential.Uid != uint32(unix.Geteuid()) || credential.Nlink != 1 || credential.Mode&0o777 != 0o600 { //nolint:gosec // Linux uid_t and Geteuid use the same UID.
					return ports.ErrAgentAccountStorageUnsafe
				}
			}
			return nil
		}
		current = filepath.Join(current, parts[i])
		next, openErr := unix.Openat(fd, parts[i], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) && (current == root || strings.HasPrefix(current, root+"/")) {
			if err := unix.Mkdirat(fd, parts[i], 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return accountProfileStorageError(err)
			}
			next, openErr = unix.Openat(fd, parts[i], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			return accountProfileStorageError(openErr)
		}
		_ = unix.Close(fd)
		fd = next
	}
}

// Only the category leaves this boundary; filesystem errors can contain paths.
func accountProfileStorageError(err error) error {
	switch {
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ENOENT):
		return ports.ErrAgentAccountStorageUnsafe
	default:
		return ports.ErrAgentAccountStorageUnavailable
	}
}

// removeAccountProfile anchors every deletion to opened directory descriptors.
// A symlink swapped into an ancestor cannot redirect cleanup outside AO; child
// links (including shared history) are unlinked, never traversed.
func removeAccountProfile(root, home string) error {
	root, home = filepath.Clean(root), filepath.Clean(home)
	rel, e := filepath.Rel(root, home)
	if e != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || !filepath.IsAbs(root) || root == "/" {
		return ports.ErrAgentAccountStorageUnsafe
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return accountProfileStorageError(e)
	}
	defer func() { _ = unix.Close(fd) }()
	current := "/"
	parts := strings.Split(strings.TrimPrefix(home, "/"), "/")
	for i, part := range parts {
		var st unix.Stat_t
		if e = unix.Fstat(fd, &st); e != nil {
			return accountProfileStorageError(e)
		}
		private := current == root || strings.HasPrefix(current, root+"/")
		owner := st.Uid == uint32(unix.Geteuid()) //nolint:gosec // Linux UID conversion.
		sticky := st.Uid == 0 && st.Mode&unix.S_ISVTX != 0
		if st.Uid != 0 && !owner || st.Mode&0o022 != 0 && !sticky || private && (!owner || st.Mode&0o777 != 0o700) {
			return ports.ErrAgentAccountStorageUnsafe
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(e, unix.ENOENT) {
			return nil
		}
		if e != nil {
			return accountProfileStorageError(e)
		}
		if i == len(parts)-1 {
			var leaf unix.Stat_t
			if e = unix.Fstat(next, &leaf); e != nil {
				_ = unix.Close(next)
				return accountProfileStorageError(e)
			}
			if leaf.Uid != uint32(unix.Geteuid()) || leaf.Mode&0o777 != 0o700 { //nolint:gosec // Linux UID conversion.
				_ = unix.Close(next)
				return ports.ErrAgentAccountStorageUnsafe
			}
			e = unlinkAccountChildren(next)
			_ = unix.Close(next)
			if e != nil {
				return e
			}
			if e = unix.Unlinkat(fd, part, unix.AT_REMOVEDIR); e != nil && !errors.Is(e, unix.ENOENT) {
				return accountProfileStorageError(e)
			}
			return nil
		}
		_ = unix.Close(fd)
		fd = next
		current = filepath.Join(current, part)
	}
	return ports.ErrAgentAccountStorageUnsafe
}
func unlinkAccountChildren(fd int) error {
	dup, e := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return accountProfileStorageError(e)
	}
	f := os.NewFile(uintptr(dup), "account-cleanup")
	entries, e := f.ReadDir(-1)
	_ = f.Close()
	if e != nil {
		return accountProfileStorageError(e)
	}
	for _, entry := range entries {
		var st unix.Stat_t
		if e = unix.Fstatat(fd, entry.Name(), &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(e, unix.ENOENT) {
			continue
		} else if e != nil {
			return accountProfileStorageError(e)
		}
		flags := 0
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			next, e := unix.Openat(fd, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if e != nil {
				return accountProfileStorageError(e)
			}
			e = unlinkAccountChildren(next)
			_ = unix.Close(next)
			if e != nil {
				return e
			}
			flags = unix.AT_REMOVEDIR
		}
		if e = unix.Unlinkat(fd, entry.Name(), flags); e != nil && !errors.Is(e, unix.ENOENT) {
			return accountProfileStorageError(e)
		}
	}
	return nil
}
