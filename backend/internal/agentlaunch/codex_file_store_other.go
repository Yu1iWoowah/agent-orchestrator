//go:build !linux

package agentlaunch

import "github.com/aoagents/agent-orchestrator/backend/internal/ports"

// ValidateCodexFileStore does not enable unverified native credential stores.
func ValidateCodexFileStore(string) error { return ports.ErrAgentAccountStorageUnsupported }
