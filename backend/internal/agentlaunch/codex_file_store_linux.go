package agentlaunch

import (
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ValidateCodexFileStore checks metadata evidence for a managed file login.
// The caller establishes AO root containment. No credential bytes are opened
// or read, and no filesystem paths escape through errors.
func ValidateCodexFileStore(home string) error {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home || home == "/" {
		return ports.ErrAgentAccountCredentialStoreUnsupported
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return ports.ErrAgentAccountStorageUnavailable
	}
	defer func() { _ = unix.Close(fd) }()
	parts := strings.Split(strings.TrimPrefix(home, "/"), "/")
	for i, part := range parts {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fileStoreEvidenceError(err)
		}
		_ = unix.Close(fd)
		fd = next
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return fileStoreEvidenceError(err)
		}
		owner := stat.Uid == uint32(unix.Geteuid()) //nolint:gosec // Linux UID conversion.
		stickyRoot := stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0
		if (!owner && stat.Uid != 0) || (stat.Mode&0o022 != 0 && !stickyRoot) || (i == len(parts)-1 && (!owner || stat.Mode&0o777 != 0o700)) {
			return ports.ErrAgentAccountCredentialStoreUnsupported
		}
	}
	var credential unix.Stat_t
	if err := unix.Fstatat(fd, "auth.json", &credential, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileStoreEvidenceError(err)
	}
	if credential.Mode&unix.S_IFMT != unix.S_IFREG || credential.Mode&0o777 != 0o600 || credential.Nlink != 1 || credential.Uid != uint32(unix.Geteuid()) { //nolint:gosec // Linux UID conversion.
		return ports.ErrAgentAccountCredentialStoreUnsupported
	}
	return nil
}

func fileStoreEvidenceError(err error) error {
	switch {
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP), errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return ports.ErrAgentAccountCredentialStoreUnsupported
	default:
		return ports.ErrAgentAccountStorageUnavailable
	}
}
