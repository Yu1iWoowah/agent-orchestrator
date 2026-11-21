//go:build linux

package hcg

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

func readLabVersion(ctx context.Context, root string, ref reporting.DelegationCredentialReference) ([]byte, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errLabTransport
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return nil, errLabTransport
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errLabTransport
	}
	defer func() { _ = unix.Close(fd) }()
	check := func(fd int, kind uint32, mode uint32) bool {
		var stat unix.Stat_t
		return unix.Fstat(fd, &stat) == nil && stat.Mode&unix.S_IFMT == kind && stat.Mode&0o777 == mode && int64(stat.Uid) == int64(os.Geteuid())
	}
	if !check(fd, unix.S_IFDIR, 0o700) {
		return nil, errLabTransport
	}
	for _, name := range []string{ref.Provider, ref.Name} {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, errLabTransport
		}
		_ = unix.Close(fd)
		fd = next
		if !check(fd, unix.S_IFDIR, 0o700) {
			return nil, errLabTransport
		}
	}
	fileFD, err := unix.Openat(fd, ref.Version+".json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errLabTransport
	}
	file := os.NewFile(uintptr(fileFD), "lab-version")
	defer func() { _ = file.Close() }()
	if !check(fileFD, unix.S_IFREG, 0o600) || ctx.Err() != nil {
		return nil, errLabTransport
	}
	body, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(body) == 0 || len(body) > 16384 || ctx.Err() != nil {
		return nil, errLabTransport
	}
	return body, nil
}
