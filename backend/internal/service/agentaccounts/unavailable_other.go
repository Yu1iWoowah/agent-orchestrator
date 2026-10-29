//go:build !linux

package agentaccounts

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Automatic cleanup requires the Linux descriptor/no-follow guarantees.
func recoverUnavailableNamespace(root string) (bool, error) {
	if _, e := os.Lstat(filepath.Join(root, ".unavailable")); errors.Is(e, os.ErrNotExist) {
		return false, nil
	}
	return false, ports.ErrAgentAccountUnavailableHomeOccupied
}
