//go:build !linux

package agentaccounts

import "github.com/aoagents/agent-orchestrator/backend/internal/ports"

// New managed profiles are Linux-only until file-only storage and the native
// platform's access controls can be verified. Never force a keyring fallback.
func ensureAccountProfile(_, _ string) error {
	return ports.ErrAgentAccountStorageUnsupported
}
