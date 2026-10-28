//go:build !windows

package appdiagnostics

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aoagents/agent-orchestrator/backend/internal/browserruntime"
)

// Listen confines the socket to the already-private diagnostics data directory.
func Listen(dataDir string) (net.Listener, string, error) {
	directory := filepath.Join(dataDir, "diagnostics")
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0o700 || int64(stat.Uid) != int64(os.Geteuid()) {
		return nil, "", errors.New("unsafe app diagnostic runtime directory")
	}
	return browserruntime.ListenNamed(filepath.Join(directory, "locator"), "app-diagnostics")
}
