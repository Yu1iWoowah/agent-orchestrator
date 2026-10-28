package agentaccount

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func prepareContained(home, provider string, dirs []string) error {
	if home == "" {
		return nil
	}
	root := filepath.Dir(filepath.Dir(home))
	if !filepath.IsAbs(home) || filepath.Clean(home) != home || filepath.Base(filepath.Dir(home)) != provider || root == "/" {
		return ports.ErrAgentAccountStorageUnsafe
	}
	homeFD, err := openContainedDir(root, home)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(homeFD) }()
	history := filepath.Join(root, "history", provider)
	historyFD, err := openContainedDir(root, history)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(historyFD) }()
	for _, name := range append(append([]string(nil), dirs...), "history.jsonl") {
		file := name == "history.jsonl"
		if file {
			fd, err := unix.Openat(historyFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
			if err != nil {
				return containedStorageError(err)
			}
			_ = unix.Close(fd)
		} else if err := unix.Mkdirat(historyFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return containedStorageError(err)
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(historyFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return containedStorageError(err)
		}
		mode, kind := uint32(0o700), uint32(unix.S_IFDIR)
		if file {
			mode, kind = 0o600, unix.S_IFREG
		}
		if stat.Mode&unix.S_IFMT != kind || stat.Mode&0o777 != mode || stat.Uid != uint32(unix.Geteuid()) || (file && stat.Nlink != 1) { //nolint:gosec // Linux UID conversion.
			return ports.ErrAgentAccountStorageUnsafe
		}
		target := filepath.Join(history, name)
		if err := unix.Symlinkat(target, homeFD, name); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return containedStorageError(err)
			}
			buf := make([]byte, len(target)+1)
			n, err := unix.Readlinkat(homeFD, name, buf)
			if err != nil || string(buf[:n]) != target {
				return ports.ErrAgentAccountStorageUnsafe
			}
		}
	}
	// A private path must never resolve to another account, history or device.
	for _, name := range []string{"auth.json", ".credentials.json", "config.toml", "settings.json", "settings.local.json", ".claude.json", "models_cache.json", "ipc", "app-server-control", "app-server-daemon"} {
		var stat unix.Stat_t
		if err := unix.Fstatat(homeFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return containedStorageError(err)
		}
		kind := stat.Mode & unix.S_IFMT
		if stat.Uid != uint32(unix.Geteuid()) || stat.Mode&0o077 != 0 || kind == unix.S_IFLNK || (kind == unix.S_IFREG && stat.Nlink != 1) { //nolint:gosec // Linux UID conversion.
			return ports.ErrAgentAccountStorageUnsafe
		}
	}
	allowed := map[string]bool{"history.jsonl": true}
	for _, dir := range dirs {
		allowed[dir] = true
	}
	return inspectPrivateTree(homeFD, allowed, 0)
}

func inspectPrivateTree(fd int, allowed map[string]bool, depth int) error {
	if depth > 64 {
		return ports.ErrAgentAccountStorageUnsafe
	}
	readFD, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return containedStorageError(err)
	}
	f := os.NewFile(uintptr(readFD), "account-directory")
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return ports.ErrAgentAccountStorageUnavailable
	}
	for _, entry := range entries {
		if allowed[entry.Name()] {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return containedStorageError(err)
		}
		kind := stat.Mode & unix.S_IFMT
		if stat.Uid != uint32(unix.Geteuid()) || kind == unix.S_IFLNK || (kind == unix.S_IFREG && stat.Nlink != 1) { //nolint:gosec // Linux UID conversion.
			return ports.ErrAgentAccountStorageUnsafe
		}
		if kind == unix.S_IFDIR {
			next, err := unix.Openat(fd, entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return containedStorageError(err)
			}
			err = inspectPrivateTree(next, nil, depth+1)
			_ = unix.Close(next)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Walk with directory descriptors: a replaced ancestor cannot redirect creation
// through a symlink. Existing directories are validated, never chmodded.
func openContainedDir(root, path string) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, containedStorageError(err)
	}
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		current = filepath.Join(current, part)
		private := current == root || strings.HasPrefix(current, root+"/")
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && private {
			if err = unix.Mkdirat(fd, part, 0o700); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		_ = unix.Close(fd)
		if err != nil {
			return -1, containedStorageError(err)
		}
		fd = next
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			_ = unix.Close(fd)
			return -1, containedStorageError(err)
		}
		owner := st.Uid == uint32(unix.Geteuid()) //nolint:gosec // Linux UID conversion.
		sticky := st.Uid == 0 && st.Mode&unix.S_ISVTX != 0
		if (!owner && st.Uid != 0) || (st.Mode&0o022 != 0 && !sticky) || (private && (!owner || st.Mode&0o777 != 0o700)) {
			_ = unix.Close(fd)
			return -1, ports.ErrAgentAccountStorageUnsafe
		}
	}
	return fd, nil
}

func containedStorageError(err error) error {
	switch {
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return ports.ErrAgentAccountStorageUnsafe
	default:
		return ports.ErrAgentAccountStorageUnavailable
	}
}
